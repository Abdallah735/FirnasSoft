package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type PacketType string

const (
	Message      PacketType = "message"
	Ping         PacketType = "ping"
	Pong         PacketType = "pong"
	FileMetadata PacketType = "file_metadata"
	FileChunk    PacketType = "file_chunk"
	Ack          PacketType = "ack"
)

const (
	chunkSize       = 32 * 1024 // Keep 32KB for safety; change to 60*1024 if needed.
	numWorkers      = 5
	resendInterval  = 2 * time.Second // Shorter for faster retry.
	maxAttempts     = 10              // More attempts for reliability.
	timeoutDuration = 60 * time.Second
	onlineThreshold = 30 * time.Second
	sendPace        = 1 * time.Millisecond // Pace sends to avoid burst loss.
)

type IncomingPacket struct {
	addr *net.UDPAddr
	data []byte
}

type Pending struct {
	data     []byte
	addr     *net.UDPAddr
	sendTime time.Time
	attempts int
}

type Client struct {
	addr     *net.UDPAddr
	lastSeen time.Time
}

// ----------------- Communication Manager -----------------
type CommunicationManager struct {
	conn     *net.UDPConn
	incoming chan IncomingPacket
}

func NewCommunicationManager(conn *net.UDPConn) *CommunicationManager {
	return &CommunicationManager{
		conn:     conn,
		incoming: make(chan IncomingPacket, 100),
	}
}

func (cm *CommunicationManager) StartListen() {
	go func() {
		buffer := make([]byte, 65536)
		for {
			n, addr, err := cm.conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading:", err)
				continue
			}
			dataCopy := make([]byte, n)
			copy(dataCopy, buffer[:n])
			cm.incoming <- IncomingPacket{addr, dataCopy}
		}
	}()
}

func (cm *CommunicationManager) Send(addr *net.UDPAddr, data []byte) error {
	_, err := cm.conn.WriteToUDP(data, addr)
	return err
}

// ----------------- Client Manager -----------------
type ClientManager struct {
	clients map[string]*Client
	mutex   sync.Mutex
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		clients: make(map[string]*Client),
	}
}

func (cm *ClientManager) UpdateClient(addr *net.UDPAddr) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	key := addr.String()
	if c, ok := cm.clients[key]; ok {
		c.lastSeen = time.Now()
	} else {
		cm.clients[key] = &Client{addr: addr, lastSeen: time.Now()}
	}
}

func (cm *ClientManager) GetClient(key string) (*net.UDPAddr, error) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	if c, ok := cm.clients[key]; ok {
		return c.addr, nil
	}
	return nil, fmt.Errorf("client not found: %s", key)
}

func (cm *ClientManager) IsOnline(key string) bool {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	if c, ok := cm.clients[key]; ok {
		return time.Since(c.lastSeen) < onlineThreshold
	}
	return false
}

func (cm *ClientManager) ListClients() []string {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	list := make([]string, 0, len(cm.clients))
	for k := range cm.clients {
		list = append(list, k)
	}
	return list
}

// ----------------- Packet Tracker -----------------
type PacketTracker struct {
	pending        map[string]*Pending
	mutex          sync.Mutex
	resendInterval time.Duration
	maxAttempts    int
	timeout        time.Duration
	comm           *CommunicationManager
}

func NewPacketTracker(comm *CommunicationManager) *PacketTracker {
	return &PacketTracker{
		pending:        make(map[string]*Pending),
		resendInterval: resendInterval,
		maxAttempts:    maxAttempts,
		timeout:        timeoutDuration,
		comm:           comm,
	}
}

func (pt *PacketTracker) Start() {
	go func() {
		for {
			time.Sleep(1 * time.Second)
			now := time.Now()
			pt.mutex.Lock()
			for key, p := range pt.pending {
				if p.attempts >= pt.maxAttempts || now.Sub(p.sendTime) > pt.timeout {
					fmt.Printf("Failed to deliver packet %s after %d attempts\n", key, p.attempts)
					delete(pt.pending, key)
					continue
				}
				if now.Sub(p.sendTime) > pt.resendInterval {
					err := pt.comm.Send(p.addr, p.data)
					if err != nil {
						fmt.Println("Error resending packet:", err)
					} else {
						p.sendTime = now
						p.attempts++
						fmt.Printf("Resending packet %s (attempt %d)\n", key, p.attempts)
					}
				}
			}
			pt.mutex.Unlock()
		}
	}()
}

func (pt *PacketTracker) AddPending(fileID string, chunkIndex int, data []byte, addr *net.UDPAddr) {
	key := fmt.Sprintf("%s_%d", fileID, chunkIndex)
	pt.mutex.Lock()
	pt.pending[key] = &Pending{data: data, addr: addr, sendTime: time.Now(), attempts: 1}
	pt.mutex.Unlock()
}

func (pt *PacketTracker) RemovePending(fileID string, chunkIndex int) {
	key := fmt.Sprintf("%s_%d", fileID, chunkIndex)
	pt.mutex.Lock()
	delete(pt.pending, key)
	pt.mutex.Unlock()
}

func (pt *PacketTracker) HasPending(fileID string, chunkIndex int) bool {
	key := fmt.Sprintf("%s_%d", fileID, chunkIndex)
	pt.mutex.Lock()
	defer pt.mutex.Unlock()
	_, ok := pt.pending[key]
	return ok
}

// ----------------- Packet Manager -----------------
type PacketManager struct {
	tracker *PacketTracker
}

func NewPacketManager(comm *CommunicationManager) *PacketManager {
	pm := &PacketManager{
		tracker: NewPacketTracker(comm),
	}
	pm.tracker.Start()
	return pm
}

func (pm *PacketManager) Generate(pt PacketType, payload map[string]interface{}) []byte {
	m := map[string]interface{}{"type": string(pt)}
	for k, v := range payload {
		if k == "data" {
			switch t := v.(type) {
			case []byte:
				m[k] = base64.StdEncoding.EncodeToString(t)
			case string:
				m[k] = t
			default:
				jsonData, err := json.Marshal(v)
				if err == nil {
					m[k] = string(jsonData)
				}
			}
			continue
		}
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		return []byte{}
	}
	return data
}

func (pm *PacketManager) Parse(data []byte) (map[string]interface{}, error) {
	m := make(map[string]interface{})
	err := json.Unmarshal(data, &m)
	if err != nil {
		return nil, fmt.Errorf("json unmarshal error: %v", err)
	}
	if _, ok := m["type"]; !ok {
		return nil, fmt.Errorf("missing type")
	}
	if d, ok := m["data"]; ok {
		if ds, ok := d.(string); ok {
			if pt, ok := m["type"].(string); ok && pt == "file_chunk" {
				b, err := base64.StdEncoding.DecodeString(ds)
				if err != nil {
					return nil, fmt.Errorf("base64 decode error: %v", err)
				}
				m["data"] = b
			}
		}
	}
	return m, nil
}

func (pm *PacketManager) Handle(s *Server, addr *net.UDPAddr, parsed map[string]interface{}) {
	ptStr, ok := parsed["type"].(string)
	if !ok {
		return
	}
	pt := PacketType(ptStr)
	switch pt {
	case Ping:
		fmt.Printf("Received PING from %s\n", addr.String())
		pong := pm.Generate(Pong, nil)
		if err := s.comm.Send(addr, pong); err != nil {
			fmt.Println("Error sending PONG:", err)
		} else {
			fmt.Printf("Sent PONG to %s\n", addr.String())
		}
	case Message:
		data, ok := parsed["data"].(string)
		if ok {
			fmt.Printf("Message from %s: %s\n", addr.String(), data)
		}
	case Ack:
		fileID, ok1 := parsed["file_id"].(string)
		chunkIndexF, ok2 := parsed["chunk_index"].(float64)
		if ok1 && ok2 {
			chunkIndex := int(chunkIndexF)
			pm.tracker.RemovePending(fileID, chunkIndex)
		}
	default:
		fmt.Printf("Unknown packet type from %s: %s\n", addr.String(), pt)
	}
}

// ----------------- Server Struct -----------------
type Server struct {
	addr   string
	comm   *CommunicationManager
	client *ClientManager
	packet *PacketManager
	conn   *net.UDPConn
}

func NewServer(addr string) *Server {
	return &Server{
		addr:   addr,
		client: NewClientManager(),
	}
}

func (s *Server) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		return fmt.Errorf("error resolving address: %v", err)
	}

	s.conn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("error listening: %v", err)
	}

	s.comm = NewCommunicationManager(s.conn)
	s.packet = NewPacketManager(s.comm)

	fmt.Println("UDP server listening on", s.addr)

	s.comm.StartListen()

	go s.processIncoming()

	go s.handleInput()

	return nil
}

func (s *Server) processIncoming() {
	for {
		inc := <-s.comm.incoming
		s.client.UpdateClient(inc.addr)
		parsed, err := s.packet.Parse(inc.data)
		if err != nil {
			fmt.Printf("Parse error from %s (len %d): %v\n", inc.addr.String(), len(inc.data), err) // Added logging for debug.
			continue
		}
		s.packet.Handle(s, inc.addr, parsed)
	}
}

func (s *Server) SendMessage(target string, msg string) error {
	addr, err := s.client.GetClient(target)
	if err != nil {
		return err
	}
	payload := map[string]interface{}{"data": msg}
	packet := s.packet.Generate(Message, payload)
	return s.comm.Send(addr, packet)
}

func splitFileData(data []byte, size int) [][]byte {
	var chunks [][]byte
	for i := 0; i < len(data); i += size {
		end := i + size
		if end > len(data) {
			end = len(data)
		}
		chunks = append(chunks, data[i:end])
	}
	return chunks
}

func (s *Server) SendFile(target string, filePath string) error {
	addr, err := s.client.GetClient(target)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	fileSize := int64(len(data))
	chunks := splitFileData(data, chunkSize)
	fileID := uuid.New().String()
	fileName := filepath.Base(filePath)
	metaPayload := map[string]interface{}{
		"file_id":      fileID,
		"total_chunks": len(chunks),
		"file_size":    fileSize,
		"file_name":    fileName,
	}
	metaPacket := s.packet.Generate(FileMetadata, metaPayload)
	if err := s.comm.Send(addr, metaPacket); err != nil {
		return err
	}
	s.packet.tracker.AddPending(fileID, -1, metaPacket, addr)

	// Wait for metadata ack with timeout.
	start := time.Now()
	for time.Since(start) < timeoutDuration {
		if !s.packet.tracker.HasPending(fileID, -1) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s.packet.tracker.HasPending(fileID, -1) {
		return fmt.Errorf("timeout waiting for metadata ack")
	}

	// Launch workers for parallel chunk sending with pacing.
	chunkCh := make(chan int, len(chunks))
	for i := 0; i < len(chunks); i++ {
		chunkCh <- i
	}
	close(chunkCh)

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			for idx := range chunkCh {
				chunkPayload := map[string]interface{}{
					"file_id":     fileID,
					"chunk_index": idx,
					"data":        chunks[idx],
				}
				packet := s.packet.Generate(FileChunk, chunkPayload)
				if err := s.comm.Send(addr, packet); err != nil {
					fmt.Printf("Error sending chunk %d: %v\n", idx, err)
				} else {
					time.Sleep(sendPace) // Pace to reduce loss.
				}
				s.packet.tracker.AddPending(fileID, idx, packet, addr)
			}
		}()
	}
	wg.Wait()
	return nil
}

func (s *Server) handleInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Server input> ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 0 {
			continue
		}
		switch parts[0] {
		case "send":
			if len(parts) < 3 {
				fmt.Println("Usage: send <clientAddr> <message>")
				continue
			}
			if err := s.SendMessage(parts[1], "[Server] "+parts[2]); err != nil {
				fmt.Println("Error:", err)
			}
		case "sendfile":
			if len(parts) < 3 {
				fmt.Println("Usage: sendfile <clientAddr> <filePath>")
				continue
			}
			if err := s.SendFile(parts[1], parts[2]); err != nil {
				fmt.Println("Error:", err)
			} else {
				fmt.Println("File send initiated")
			}
		case "list":
			clients := s.client.ListClients()
			for _, c := range clients {
				fmt.Println("Client:", c)
			}
		case "isonline":
			if len(parts) < 2 {
				fmt.Println("Usage: isonline <clientAddr>")
				continue
			}
			online := s.client.IsOnline(parts[1])
			fmt.Printf("%s is online: %t\n", parts[1], online)
		}
	}
}

func main() {
	server := NewServer(":10000")
	if err := server.Start(); err != nil {
		fmt.Println("Server error:", err)
	}
	select {}
}

// package main

// import (
// 	"bufio"
// 	"encoding/json"
// 	"fmt"
// 	"net"
// 	"os"
// 	"strings"
// )

// type commandType int

// const (
// 	addClient commandType = iota
// 	sendMessage
// 	listClients
// 	getClient
// )

// type command struct {
// 	typ       commandType
// 	addr      *net.UDPAddr
// 	targetKey string
// 	message   any
// 	replyCh   chan any
// 	errCh     chan error
// }

// // ----------------- Server Struct -----------------
// type Server struct {
// 	addr     string
// 	conn     *net.UDPConn
// 	commands chan *command
// }

// // Create new server
// func NewServer(addr string) *Server {
// 	return &Server{
// 		addr:     addr,
// 		commands: make(chan *command),
// 	}
// }

// // Start server
// func (s *Server) Start() error {
// 	udpAddr, err := net.ResolveUDPAddr("udp", s.addr)
// 	if err != nil {
// 		return fmt.Errorf("Error resolving address: %v", err)
// 	}

// 	s.conn, err = net.ListenUDP("udp", udpAddr)
// 	if err != nil {
// 		return fmt.Errorf("Error listening: %v", err)
// 	}

// 	fmt.Println("UDP server listening on", s.addr)

// 	// Start manager
// 	go s.clientManagerWorker()

// 	// Start console input
// 	go s.handleInput()

// 	// Start handling packets
// 	s.handlePackets()

// 	return nil
// }

// // Manager goroutine
// func (s *Server) clientManagerWorker() {
// 	clients := make(map[string]*net.UDPAddr)

// 	for {
// 		cmd := <-s.commands

// 		switch cmd.typ {
// 		case addClient:
// 			clients[cmd.addr.String()] = cmd.addr
// 			cmd.errCh <- nil

// 		case sendMessage:
// 			if client, ok := clients[cmd.targetKey]; ok {
// 				var data []byte
// 				switch v := cmd.message.(type) {
// 				case string:
// 					data = []byte(v)
// 				case []byte:
// 					data = v
// 				default:
// 					jsonData, err := json.Marshal(v)
// 					if err != nil {
// 						cmd.errCh <- fmt.Errorf("unsupported message type: %T", v)
// 						continue
// 					}
// 					data = jsonData
// 				}

// 				n, err := s.conn.WriteToUDP(data, client)
// 				if err != nil {
// 					cmd.errCh <- err
// 					continue
// 				}
// 				if n != len(data) {
// 					cmd.errCh <- fmt.Errorf("incomplete write: wrote %d of %d", n, len(data))
// 					continue
// 				}
// 				cmd.errCh <- nil
// 			} else {
// 				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
// 			}

// 		case listClients:
// 			list := make([]string, 0, len(clients))
// 			for k := range clients {
// 				list = append(list, k)
// 			}
// 			cmd.replyCh <- list

// 		case getClient:
// 			if client, ok := clients[cmd.targetKey]; ok {
// 				cmd.replyCh <- client
// 			} else {
// 				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
// 			}
// 		}
// 	}
// }

// // Send message
// func (s *Server) SendMessage(target string, msg any) error {
// 	errCh := make(chan error)
// 	s.commands <- &command{
// 		typ:       sendMessage,
// 		targetKey: target,
// 		message:   msg,
// 		errCh:     errCh,
// 	}
// 	return <-errCh
// }

// // List clients
// func (s *Server) ListClients() ([]string, error) {
// 	replyCh := make(chan any)
// 	errCh := make(chan error)

// 	s.commands <- &command{
// 		typ:     listClients,
// 		replyCh: replyCh,
// 		errCh:   errCh,
// 	}

// 	select {
// 	case data := <-replyCh:
// 		return data.([]string), nil
// 	case err := <-errCh:
// 		return nil, err
// 	}
// }

// // Get client
// func (s *Server) GetClient(addr string) (*net.UDPAddr, error) {
// 	replyCh := make(chan any)
// 	errCh := make(chan error)

// 	s.commands <- &command{
// 		typ:       getClient,
// 		targetKey: addr,
// 		replyCh:   replyCh,
// 		errCh:     errCh,
// 	}

// 	select {
// 	case data := <-replyCh:
// 		return data.(*net.UDPAddr), nil
// 	case err := <-errCh:
// 		return nil, err
// 	}
// }

// // Add client
// func (s *Server) AddClient(addr *net.UDPAddr) error {
// 	errCh := make(chan error)
// 	s.commands <- &command{
// 		typ:   addClient,
// 		addr:  addr,
// 		errCh: errCh,
// 	}
// 	return <-errCh
// }

// // Handle console input
// func (s *Server) handleInput() {
// 	reader := bufio.NewReader(os.Stdin)
// 	for {
// 		fmt.Print("Server input> ")
// 		line, _ := reader.ReadString('\n')
// 		line = strings.TrimSpace(line)

// 		if strings.HasPrefix(line, "send ") {
// 			parts := strings.SplitN(line, " ", 3)
// 			if len(parts) < 3 {
// 				fmt.Println("Usage: send <clientAddr> <message>")
// 				continue
// 			}
// 			if err := s.SendMessage(parts[1], "[Server] "+parts[2]); err != nil {
// 				fmt.Println("Error:", err)
// 			}
// 		} else if line == "list" {
// 			clients, err := s.ListClients()
// 			if err != nil {
// 				fmt.Println("Error:", err)
// 				continue
// 			}
// 			for _, c := range clients {
// 				fmt.Println("Client:", c)
// 			}
// 		}
// 	}
// }

// // Handle incoming packets
// func (s *Server) handlePackets() {
// 	buffer := make([]byte, 1024)
// 	for {
// 		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
// 		if err != nil {
// 			fmt.Println("Error reading:", err)
// 			continue
// 		}

// 		// Register client
// 		if err := s.AddClient(clientAddr); err != nil {
// 			fmt.Println("Error adding client:", err)
// 		}

// 		// Print message
// 		msg := strings.TrimSpace(string(buffer[:n]))
// 		if msg == "PING" {
// 			s.SendMessage(clientAddr.String(), "PONG")
// 		}
// 		fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
// 	}
// }

// func main() {
// 	server := NewServer(":10000")
// 	if err := server.Start(); err != nil {
// 		fmt.Println("Server error:", err)
// 	}
// }
