// === server.go ===
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	chunkSize       = 32 * 1024 // 32KB
	numWorkers      = 6
	resendInterval  = 2 * time.Second
	maxAttempts     = 10
	timeoutDuration = 60 * time.Second
	onlineThreshold = 30 * time.Second
	sendPace        = 40 * time.Millisecond
)

// IncomingPacket = raw data + addr
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
	mutex   chan struct{} // simple serialized access
}

func NewClientManager() *ClientManager {
	return &ClientManager{
		clients: make(map[string]*Client),
		mutex:   make(chan struct{}, 1),
	}
}

func (cm *ClientManager) lock()   { cm.mutex <- struct{}{} }
func (cm *ClientManager) unlock() { <-cm.mutex }

func (cm *ClientManager) UpdateClient(addr *net.UDPAddr) {
	cm.lock()
	defer cm.unlock()
	key := addr.String()
	if c, ok := cm.clients[key]; ok {
		c.lastSeen = time.Now()
	} else {
		cm.clients[key] = &Client{addr: addr, lastSeen: time.Now()}
	}
}

func (cm *ClientManager) GetClient(key string) (*net.UDPAddr, error) {
	cm.lock()
	defer cm.unlock()
	if c, ok := cm.clients[key]; ok {
		return c.addr, nil
	}
	return nil, fmt.Errorf("client not found: %s", key)
}

func (cm *ClientManager) IsOnline(key string) bool {
	cm.lock()
	defer cm.unlock()
	if c, ok := cm.clients[key]; ok {
		return time.Since(c.lastSeen) < onlineThreshold
	}
	return false
}

func (cm *ClientManager) ListClients() []string {
	cm.lock()
	defer cm.unlock()
	list := make([]string, 0, len(cm.clients))
	for k := range cm.clients {
		list = append(list, k)
	}
	return list
}

// ----------------- Packet Tracker (actor-style) -----------------

type addReq struct {
	fileID     string
	chunkIndex int
	data       []byte
	addr       *net.UDPAddr
	ackCh      chan struct{} // if non-nil, close when ack received (or failure)
}
type removeReq struct {
	fileID     string
	chunkIndex int
}
type hasReq struct {
	fileID     string
	chunkIndex int
	resp       chan bool
}
type hasAnyReq struct {
	fileID string
	resp   chan bool
}

type SendTask struct {
	fileID     string
	chunkIndex int
	data       []byte
	addr       *net.UDPAddr
}

type PacketTracker struct {
	// internal state (owned by single goroutine)
	pending map[string]*Pending

	// channels for commands
	addCh    chan addReq
	removeCh chan removeReq
	hasCh    chan hasReq
	hasAnyCh chan hasAnyReq

	// queue to workers who actually write to UDP
	sendQueue chan SendTask

	// config & deps
	resendInterval time.Duration
	maxAttempts    int
	timeout        time.Duration
	comm           *CommunicationManager
	waiters        map[string][]chan struct{} // ack waiters per key
	workerCount    int
}

func NewPacketTracker(comm *CommunicationManager, workerCount int) *PacketTracker {
	return &PacketTracker{
		pending:        make(map[string]*Pending),
		addCh:          make(chan addReq, 200),
		removeCh:       make(chan removeReq, 200),
		hasCh:          make(chan hasReq),
		hasAnyCh:       make(chan hasAnyReq),
		sendQueue:      make(chan SendTask, 1000),
		resendInterval: resendInterval,
		maxAttempts:    maxAttempts,
		timeout:        timeoutDuration,
		comm:           comm,
		waiters:        make(map[string][]chan struct{}),
		workerCount:    workerCount,
	}
}

func (pt *PacketTracker) Start() {
	// start workers that perform actual network writes
	for i := 0; i < pt.workerCount; i++ {
		go func(id int) {
			for task := range pt.sendQueue {
				if err := pt.comm.Send(task.addr, task.data); err != nil {
					fmt.Printf("[send worker %d] Error sending packet %s_%d: %v\n", id, task.fileID, task.chunkIndex, err)
				} else {
					fmt.Printf("[send worker %d] Sent %s_%d -> %s\n", id, task.fileID, task.chunkIndex, task.addr.String())
				}
				// throttle a little to reduce bursts
				time.Sleep(sendPace)
			}
		}(i)
	}

	// single goroutine that owns `pending` and handles resends / commands
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case req := <-pt.addCh:
				key := fmt.Sprintf("%s_%d", req.fileID, req.chunkIndex)
				// add into pending and enqueue to sendQueue
				pt.pending[key] = &Pending{data: req.data, addr: req.addr, sendTime: time.Now(), attempts: 1}
				if req.ackCh != nil {
					pt.waiters[key] = append(pt.waiters[key], req.ackCh)
				}
				pt.sendQueue <- SendTask{fileID: req.fileID, chunkIndex: req.chunkIndex, data: req.data, addr: req.addr}
				fmt.Printf("[Tracker] Added pending %s (attempt=1)\n", key)

			case rem := <-pt.removeCh:
				key := fmt.Sprintf("%s_%d", rem.fileID, rem.chunkIndex)
				if _, ok := pt.pending[key]; ok {
					delete(pt.pending, key)
					fmt.Printf("[Tracker] Removed pending %s (ACK received)\n", key)
				}
				// notify waiters if any
				if chans, ok := pt.waiters[key]; ok {
					for _, ch := range chans {
						// close per-waiter chan to notify waiter (only once per waiter)
						select {
						case <-ch:
							// already closed / consumed (defensive)
						default:
							close(ch)
						}
					}
					delete(pt.waiters, key)
				}

			case h := <-pt.hasCh:
				key := fmt.Sprintf("%s_%d", h.fileID, h.chunkIndex)
				_, ok := pt.pending[key]
				h.resp <- ok

			case ha := <-pt.hasAnyCh:
				prefix := ha.fileID + "_"
				found := false
				for k := range pt.pending {
					if strings.HasPrefix(k, prefix) {
						found = true
						break
					}
				}
				ha.resp <- found

			case <-ticker.C:
				now := time.Now()
				for key, p := range pt.pending {
					if p.attempts >= pt.maxAttempts || now.Sub(p.sendTime) > pt.timeout {
						fmt.Printf("Failed to deliver packet %s after %d attempts\n", key, p.attempts)
						// notify waiters of failure (close)
						if chans, ok := pt.waiters[key]; ok {
							for _, ch := range chans {
								select {
								case <-ch:
								default:
									close(ch)
								}
							}
							delete(pt.waiters, key)
						}
						delete(pt.pending, key)
						continue
					}
					if now.Sub(p.sendTime) > pt.resendInterval {
						// increment attempts and requeue
						p.attempts++
						p.sendTime = now
						// recover fileID and chunkIndex from key
						parts := strings.Split(key, "_")
						if len(parts) >= 2 {
							idxStr := parts[len(parts)-1]
							idx, err := strconv.Atoi(idxStr)
							fileID := strings.Join(parts[:len(parts)-1], "_")
							if err == nil {
								fmt.Printf("Resending packet %s (attempt %d)\n", key, p.attempts)
								pt.sendQueue <- SendTask{fileID: fileID, chunkIndex: idx, data: p.data, addr: p.addr}
							}
						}
					}
				}
			}
		}
	}()
}

// Add a packet for sending (non-blocking). Tracker will enqueue it and manage retries.
func (pt *PacketTracker) Add(fileID string, chunkIndex int, data []byte, addr *net.UDPAddr) {
	pt.addCh <- addReq{fileID: fileID, chunkIndex: chunkIndex, data: data, addr: addr, ackCh: nil}
}

// Add and wait for ack (blocks until ack or timeout). Useful for metadata handshake.
func (pt *PacketTracker) AddAndWaitAck(fileID string, chunkIndex int, data []byte, addr *net.UDPAddr, timeout time.Duration) error {
	ackCh := make(chan struct{})
	pt.addCh <- addReq{fileID: fileID, chunkIndex: chunkIndex, data: data, addr: addr, ackCh: ackCh}
	select {
	case <-ackCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for ack %s_%d", fileID, chunkIndex)
	}
}

// NotifyAck should be called when an ACK packet is received (removes pending + notifies waiters).
func (pt *PacketTracker) NotifyAck(fileID string, chunkIndex int) {
	pt.removeCh <- removeReq{fileID: fileID, chunkIndex: chunkIndex}
}

func (pt *PacketTracker) HasPending(fileID string, chunkIndex int) bool {
	resp := make(chan bool)
	pt.hasCh <- hasReq{fileID: fileID, chunkIndex: chunkIndex, resp: resp}
	return <-resp
}

func (pt *PacketTracker) HasAnyPending(fileID string) bool {
	resp := make(chan bool)
	pt.hasAnyCh <- hasAnyReq{fileID: fileID, resp: resp}
	return <-resp
}

// ----------------- Packet Manager -----------------
type PacketManager struct {
	tracker *PacketTracker
}

func NewPacketManager(comm *CommunicationManager, workerCount int) *PacketManager {
	pt := NewPacketTracker(comm, workerCount)
	pt.Start()
	return &PacketManager{tracker: pt}
}

func (pm *PacketManager) Generate(ptype PacketType, payload map[string]interface{}) []byte {
	m := map[string]interface{}{"type": string(ptype)}
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
	ptype := PacketType(ptStr)
	switch ptype {
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
			// notify tracker to remove pending and wake waiters
			pm.tracker.NotifyAck(fileID, chunkIndex)
			// update server-side progress and logs
			if s != nil {
				s.mu.Lock()
				defer s.mu.Unlock()
				if chunkIndex == -1 {
					fmt.Printf("[ACK-META] metadata for file %s from %s\n", fileID, addr.String())
				} else {
					s.fileAcked[fileID]++
					acked := s.fileAcked[fileID]
					total := s.fileTotal[fileID]
					fmt.Printf("[ACK] file %s chunk %d from %s (acked %d/%d)\n", fileID, chunkIndex, addr.String(), acked, total)
				}
			}
		}
	default:
		fmt.Printf("Unknown packet type from %s: %s\n", addr.String(), ptype)
	}
}

// ----------------- Server Struct -----------------
type Server struct {
	addr   string
	comm   *CommunicationManager
	client *ClientManager
	packet *PacketManager
	conn   *net.UDPConn

	// progress tracking
	mu           sync.Mutex
	fileTotal    map[string]int
	fileEnqueued map[string]int
	fileAcked    map[string]int
}

func NewServer(addr string) *Server {
	return &Server{
		addr:         addr,
		client:       NewClientManager(),
		fileTotal:    make(map[string]int),
		fileEnqueued: make(map[string]int),
		fileAcked:    make(map[string]int),
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
	s.packet = NewPacketManager(s.comm, numWorkers)

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
			fmt.Printf("Parse error from %s (len %d): %v\n", inc.addr.String(), len(inc.data), err)
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
	// Direct send for simple messages (no ACK expected)
	if err := s.comm.Send(addr, packet); err != nil {
		return err
	}
	fmt.Printf("[SEND] Message -> %s : %s\n", target, msg)
	return nil
}

// SendFile now streams file from disk chunk-by-chunk, sends metadata and waits for ACK before sending chunks.
// metadata uses chunkIndex = -1
func (s *Server) SendFile(target string, filePath string) error {
	addr, err := s.client.GetClient(target)
	if err != nil {
		return err
	}

	fi, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	fileSize := fi.Size()
	totalChunks := int((fileSize + int64(chunkSize) - 1) / int64(chunkSize))
	fileID := uuid.New().String()
	fileName := filepath.Base(filePath)

	metaPayload := map[string]interface{}{
		"file_id":      fileID,
		"total_chunks": totalChunks,
		"file_size":    fileSize,
		"file_name":    fileName,
	}

	metaPacket := s.packet.Generate(FileMetadata, metaPayload)

	// record progress counters
	s.mu.Lock()
	s.fileTotal[fileID] = totalChunks
	s.fileEnqueued[fileID] = 0
	s.fileAcked[fileID] = 0
	s.mu.Unlock()

	fmt.Printf("[SEND-FILE] Sending METADATA %s -> %s (chunks=%d size=%d)\n", fileID, target, totalChunks, fileSize)

	// Add metadata and wait for its ACK before sending chunks (generator behavior you wanted).
	if err := s.packet.tracker.AddAndWaitAck(fileID, -1, metaPacket, addr, timeoutDuration); err != nil {
		return fmt.Errorf("metadata ack timeout: %w", err)
	}

	fmt.Printf("[SEND-FILE] METADATA ACK received for %s -> %s\n", fileID, target)

	// Open file and stream chunks
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, chunkSize)
	for idx := 0; idx < totalChunks; idx++ {
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		chunkData := make([]byte, n)
		copy(chunkData, buf[:n])

		chunkPayload := map[string]interface{}{
			"file_id":     fileID,
			"chunk_index": idx,
			"data":        chunkData,
		}
		packet := s.packet.Generate(FileChunk, chunkPayload)
		// queue chunk to tracker (non-blocking). tracker will enqueue to sendQueue and manage retries.
		s.packet.tracker.Add(fileID, idx, packet, addr)
		// update enqueued counter and log
		s.mu.Lock()
		s.fileEnqueued[fileID]++
		enq := s.fileEnqueued[fileID]
		tot := s.fileTotal[fileID]
		s.mu.Unlock()
		fmt.Printf("[SEND-FILE] Enqueued chunk %d/%d for file %s -> %s\n", enq, tot, fileID, target)
	}

	// wait for all pending of this file to clear (acks or timeout)
	start := time.Now()
	for time.Since(start) < timeoutDuration {
		if !s.packet.tracker.HasAnyPending(fileID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s.packet.tracker.HasAnyPending(fileID) {
		return fmt.Errorf("timeout waiting for file %s completion", fileID)
	}

	fmt.Printf("[SEND-FILE] File %s completed (acked %d/%d)\n", fileID, s.fileAcked[fileID], s.fileTotal[fileID])

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
				fmt.Println("File send initiated and completed (or timed out)")
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
