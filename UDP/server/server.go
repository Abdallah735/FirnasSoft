// === server.go (modified) ===
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Register         = 1
	Ping             = 2
	Message          = 3
	Ack              = 4
	Metadata         = 5
	Chunk            = 6
	RequestChunk     = 7
	PendingChunk     = 8
	TransferComplete = 9

	ChunkSize = 1400 //1200
)

type Job struct {
	Addr   *net.UDPAddr
	Packet []byte
}

type GenTask struct {
	Addr              *net.UDPAddr
	MsgType           byte
	Payload           []byte
	ClientAckPacketId uint16
	AckChan           chan struct{}
}

type PendingPacketsJob struct {
	Job
	LastSend time.Time
}

type Client struct {
	ID   string
	Addr *net.UDPAddr
}

type FileMeta struct {
	Filename    string
	TotalChunks int
	ChunkSize   int
	Received    int
}

type Mutex struct {
	Action   string
	Addr     *net.UDPAddr
	Id       string
	Packet   []byte
	PacketID uint16
	Reply    chan interface{}
	AckChan  chan struct{}
}

type Server struct {
	conn           *net.UDPConn
	clientsByID    map[string]*Client
	clientsByAddr  map[string]*Client
	writeQueue     chan Job
	pendingPackets map[uint16]PendingPacketsJob
	parseQueue     chan Job
	genQueue       chan GenTask
	builtpackets   chan Job
	muxPending     chan Mutex
	muxClient      chan Mutex
	metaPendingMap map[uint16]chan struct{}

	snapshot atomic.Value

	packetIDCounter uint32
	//
	filesMu        sync.Mutex
	files          map[string]*os.File // incoming files from clients (receiving)
	meta           map[string]FileMeta
	receivedChunks map[string]map[int]bool

	// files to serve (when server acts as sender)
	serveMux   sync.Mutex
	serveFiles map[string]*os.File
	serveMeta  map[string]FileMeta

	// waiters for received chunks per filename per client address
	waitMux   sync.Mutex
	waitChans map[string]map[int]chan struct{}
}

func NewServer(addr string) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		conn:           conn,
		clientsByID:    make(map[string]*Client),
		clientsByAddr:  make(map[string]*Client),
		writeQueue:     make(chan Job, 5000),
		pendingPackets: make(map[uint16]PendingPacketsJob),
		parseQueue:     make(chan Job, 5000),
		genQueue:       make(chan GenTask, 5000),
		builtpackets:   make(chan Job, 5000),
		muxPending:     make(chan Mutex, 5000),
		muxClient:      make(chan Mutex, 5000),
		metaPendingMap: make(map[uint16]chan struct{}),
		files:          make(map[string]*os.File),
		meta:           make(map[string]FileMeta),
		receivedChunks: make(map[string]map[int]bool),

		serveFiles: make(map[string]*os.File),
		serveMeta:  make(map[string]FileMeta),
		waitChans:  make(map[string]map[int]chan struct{}),
	}
	s.snapshot.Store(make(map[uint16]PendingPacketsJob))
	return s, nil
}

func (s *Server) udpWriteWorker(id int) {
	for {
		job := <-s.writeQueue
		_, err := s.conn.WriteToUDP(job.Packet, job.Addr)
		if err != nil {
			fmt.Printf("Writer %d error: %v\n", id, err)
		}
	}
}

func (s *Server) udpReadWorker() {
	//buf := make([]byte, 65507)
	for {
		buf := make([]byte, 65507)
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Read error:", err)
			continue
		}
		packet := make([]byte, n)
		copy(packet, buf[:n])
		s.parseQueue <- Job{Addr: addr, Packet: packet}
	}
}

func (s *Server) packetSender() {
	for {
		job := <-s.builtpackets
		s.writeQueue <- job
	}
}

func (s *Server) handleRegister(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	id := string(payload)
	s.muxClient <- Mutex{Action: "registration", Addr: addr, Id: id}
	s.packetGenerator(addr, Ack, []byte("Registered success"), clientAckPacketId, nil)
	fmt.Println("Registered client:", id, addr)
}

func (s *Server) getClientByAddr(addr *net.UDPAddr) *Client {
	reply := make(chan interface{})
	s.muxClient <- Mutex{Action: "clientByAddr", Addr: addr, Reply: reply}
	client := (<-reply).(*Client)
	return client
}

func (s *Server) getClientById(id string) *Client {
	reply := make(chan interface{})
	s.muxClient <- Mutex{Action: "clientByID", Id: id, Reply: reply}
	client, _ := (<-reply).(*Client)
	return client
}

func (s *Server) handlePing(addr *net.UDPAddr, clientAckPacketId uint16) {
	client := s.getClientByAddr(addr)
	if client == nil {
		fmt.Println("Ping from unknown client:", addr)
		return
	}
	s.packetGenerator(addr, Ack, []byte("pong"), clientAckPacketId, nil)
	fmt.Printf("Ping from %s\n", client.ID)
}

func (s *Server) handleMessage(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	client := s.getClientByAddr(addr)
	if client == nil {
		fmt.Println("Message from unknown client:", addr)
		return
	}
	s.packetGenerator(addr, Ack, []byte("message received"), clientAckPacketId, nil)
	fmt.Printf("Message from %s: %s\n", client.ID, string(payload))
}

func (s *Server) packetGenerator(addr *net.UDPAddr, msgType byte, payload []byte, clientAckPacketId uint16, ackChan chan struct{}) {
	task := GenTask{Addr: addr, MsgType: msgType, Payload: payload, ClientAckPacketId: clientAckPacketId, AckChan: ackChan}
	s.genQueue <- task
}

func (s *Server) pktGWorker() {
	for {
		task := <-s.genQueue
		packet := make([]byte, 2+2+1+len(task.Payload))

		pid := atomic.AddUint32(&s.packetIDCounter, 1)
		packetID := uint16(pid & 0xFFFF)

		binary.BigEndian.PutUint16(packet[2:4], 0)
		packet[4] = task.MsgType
		copy(packet[5:], task.Payload)

		if task.MsgType != Ack && task.MsgType != RequestChunk && task.MsgType != PendingChunk && task.MsgType != TransferComplete {
			binary.BigEndian.PutUint16(packet[0:2], packetID)
			s.muxPending <- Mutex{Action: "addPending", PacketID: packetID, Addr: task.Addr, Packet: packet}
			if task.AckChan != nil {
				s.muxClient <- Mutex{Action: "registerAckMetadata", PacketID: packetID, AckChan: task.AckChan}
			}
		} else {
			binary.BigEndian.PutUint16(packet[0:2], task.ClientAckPacketId)
		}

		s.builtpackets <- Job{Addr: task.Addr, Packet: packet}
	}
}

func (s *Server) packetParserWorker() {
	for {
		job := <-s.parseQueue
		s.PacketParser(job.Addr, job.Packet)
	}
}

func (s *Server) PacketParser(addr *net.UDPAddr, packet []byte) {
	if len(packet) < 5 {
		return
	}

	packetID := binary.BigEndian.Uint16(packet[0:2])
	binary.BigEndian.PutUint16(packet[2:4], 0)
	msgType := packet[4]
	payload := packet[5:]

	switch msgType {
	case Register:
		s.handleRegister(addr, payload, packetID)
	case Ping:
		s.handlePing(addr, packetID)
	case Message:
		s.handleMessage(addr, payload, packetID)
	case Ack:
		s.handleAck(packetID, payload)
	case Metadata:
		s.handleMetadata(addr, payload, packetID)
	case Chunk:
		s.handleChunk(addr, payload, packetID)
	case RequestChunk:
		s.handleRequestChunk(addr, payload, packetID)
	case PendingChunk:
		// receiver told us pending; we can ignore or log
		fmt.Println("Received pending from", addr, string(payload))
	case TransferComplete:
		s.handleTransferComplete(addr, payload, packetID)
	}
}

func (s *Server) handleMetadata(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	// payload: filename|totalChunks|chunkSize
	parts := strings.Split(string(payload), "|")
	if len(parts) != 3 {
		fmt.Println("Invalid metadata from", addr.String())
		return
	}
	filename := parts[0]
	totalChunks, _ := strconv.Atoi(parts[1])
	chunkSz, _ := strconv.Atoi(parts[2])

	// prepare file for this addr (this is incoming file from client)
	key := addr.String()
	fpath := "fromClient_" + filename

	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	//should be before file opening
	s.filesMu.Lock()
	if _, ok := s.files[key]; ok {
		s.filesMu.Unlock()
		fmt.Printf("Duplicate metadata ignored from %s\n", addr.String())
		return
	}

	s.files[key] = f
	s.meta[key] = FileMeta{
		Filename:    filename,
		TotalChunks: totalChunks,
		ChunkSize:   chunkSz,
		Received:    0,
	}
	s.receivedChunks[key] = make(map[int]bool)
	s.filesMu.Unlock()

	// ack metadata back to client (client is expecting it)
	s.packetGenerator(addr, Ack, []byte("metadata received"), clientAckPacketId, nil)
	fmt.Printf("Metadata received from %s: %s (%d chunks, %d bytes each)\n", addr.String(), filename, totalChunks, chunkSz)

	// start request manager to pull chunks from this client
	go s.requestManagerForIncoming(addr, filename, totalChunks, chunkSz)
}

func (s *Server) handleChunk(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	data := make([]byte, len(payload)-4)
	copy(data, payload[4:])

	key := addr.String()

	s.filesMu.Lock()
	if _, exists := s.receivedChunks[key]; !exists {
		s.receivedChunks[key] = make(map[int]bool)
	}

	if s.receivedChunks[key][idx] {
		s.filesMu.Unlock()
		fmt.Printf("Duplicate chunk %d ignored from %s\n", idx, key)
		s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d already received", idx)), clientAckPacketId, nil)
		return
	}
	s.receivedChunks[key][idx] = true

	f, okf := s.files[key]
	meta, okm := s.meta[key]
	if okm {
		meta.Received++
		s.meta[key] = meta
	}
	s.filesMu.Unlock()

	if !okf {
		fmt.Println("No file handle for", key)
	}

	offset := int64(idx * meta.ChunkSize)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}

	// notify waiting channels for this client+file if any
	name := meta.Filename
	s.waitMux.Lock()
	if chmap, ok := s.waitChans[name]; ok {
		if ch, ok2 := chmap[idx]; ok2 {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}
	s.waitMux.Unlock()

	// ack the chunk to client
	s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil)
	fmt.Printf("Chunk %d received from %s (%d/%d)\n", idx, addr.String(), meta.Received, meta.TotalChunks)

	// if done, close
	if okm && meta.Received >= meta.TotalChunks {
		s.filesMu.Lock()
		f.Close()
		delete(s.files, key)
		delete(s.meta, key)
		delete(s.receivedChunks, key)
		s.filesMu.Unlock()
		fmt.Printf("File saved from %s: fromClient_%s\n", addr.String(), meta.Filename)
		// send transfer_complete back to sender? the receiver is server in this case, but protocol expects receiver to send it.
	}
}

func (s *Server) fieldPacketTrackingWorker() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		reply := make(chan interface{})
		s.muxPending <- Mutex{Action: "getAllPending", Reply: reply}
		pendings := (<-reply).(map[uint16]PendingPacketsJob)

		for packetID, pending := range pendings {
			if now.Sub(pending.LastSend) >= 1*time.Second {
				s.builtpackets <- pending.Job
				s.muxPending <- Mutex{Action: "updatePending", PacketID: packetID}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func (s *Server) handleAck(packetID uint16, payload []byte) {
	fmt.Println("Client ack:", string(payload))
	s.muxPending <- Mutex{Action: "deletePending", PacketID: packetID}
}

func (s *Server) SendFileToClient(client *Client, filepath string, filename string) error {
	// open and register to serve file on request
	f, err := os.Open(filepath)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	fileSize := stat.Size()
	totalChunks := int((fileSize + int64(ChunkSize) - 1) / int64(ChunkSize))

	// save serve file
	s.serveMux.Lock()
	s.serveFiles[filename] = f
	s.serveMeta[filename] = FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: ChunkSize}
	s.serveMux.Unlock()

	metadataStr := fmt.Sprintf("%s|%d|%d", filename, totalChunks, ChunkSize)
	metaAck := make(chan struct{})
	s.packetGenerator(client.Addr, Metadata, []byte(metadataStr), 0, metaAck)

	select {
	case <-metaAck:
		fmt.Println("Metadata ack received, waiting for chunk requests")
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting metadata ack")
	}
	return nil
}

func (s *Server) handleRequestChunk(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	// payload: 4-byte idx + filename
	if len(payload) < 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	filename := string(payload[4:])

	s.serveMux.Lock()
	f, okf := s.serveFiles[filename]
	meta, okm := s.serveMeta[filename]
	s.serveMux.Unlock()

	if !okf || !okm {
		fmt.Printf("Request for unknown file %s\n", filename)
		// inform pending
		s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil)
		return
	}

	offset := int64(idx * meta.ChunkSize)
	buf := make([]byte, meta.ChunkSize)
	_, err := f.ReadAt(buf, offset)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			stat, stErr := f.Stat()
			if stErr == nil {
				fileSize := stat.Size()
				start := offset
				if start >= fileSize {
					s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil)
					return
				}
				end := start + int64(meta.ChunkSize)
				if end > fileSize {
					end = fileSize
				}
				buf = buf[:end-start]
			} else {
				s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil)
				return
			}
		} else {
			s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil)
			return
		}
	}

	payloadChunk := make([]byte, 4+len(buf))
	binary.BigEndian.PutUint32(payloadChunk[0:4], uint32(idx))
	copy(payloadChunk[4:], buf)

	s.packetGenerator(addr, Chunk, payloadChunk, 0, nil)
}

func (s *Server) handleTransferComplete(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	filename := string(payload)
	// cleanup serve resources if present
	s.serveMux.Lock()
	if f, ok := s.serveFiles[filename]; ok {
		f.Close()
		delete(s.serveFiles, filename)
	}
	delete(s.serveMeta, filename)
	s.serveMux.Unlock()
	fmt.Printf("Peer %s reports transfer complete for %s\n", addr.String(), filename)
}

func (s *Server) MutexHandleClientActions() {
	for mu := range s.muxClient {
		switch mu.Action {
		case "registration":
			client := &Client{ID: mu.Id, Addr: mu.Addr}
			s.clientsByID[mu.Id] = client
			s.clientsByAddr[mu.Addr.String()] = client

		case "clientByAddr":
			mu.Reply <- s.clientsByAddr[mu.Addr.String()]

		case "clientByID":
			mu.Reply <- s.clientsByID[mu.Id]

		case "registerAckMetadata":
			if mu.AckChan != nil {
				s.metaPendingMap[mu.PacketID] = mu.AckChan
			}
		}
	}
}

func (s *Server) MutexHandleActions() {
	for mu := range s.muxPending {
		switch mu.Action {
		case "addPending":
			s.pendingPackets[mu.PacketID] = PendingPacketsJob{
				Job:      Job{Addr: mu.Addr, Packet: mu.Packet},
				LastSend: time.Now(),
			}
			s.updatePendingSnapshot()

		case "updatePending":
			if p, ok := s.pendingPackets[mu.PacketID]; ok {
				p.LastSend = time.Now()
				s.pendingPackets[mu.PacketID] = p
				s.updatePendingSnapshot()
			}

		case "deletePending":
			delete(s.pendingPackets, mu.PacketID)
			if ch, ok := s.metaPendingMap[mu.PacketID]; ok {
				close(ch)
				delete(s.metaPendingMap, mu.PacketID)
			}
			s.updatePendingSnapshot()

		case "getAllPending":
			snap := s.snapshot.Load()
			if snap == nil {
				mu.Reply <- make(map[uint16]PendingPacketsJob)
			} else {
				mu.Reply <- snap.(map[uint16]PendingPacketsJob)
			}
		}
	}
}

func (s *Server) updatePendingSnapshot() {
	cp := make(map[uint16]PendingPacketsJob, len(s.pendingPackets))
	for k, v := range s.pendingPackets {
		cp[k] = v
	}
	s.snapshot.Store(cp)
}

// request manager for incoming file from a specific client
func (s *Server) requestManagerForIncoming(addr *net.UDPAddr, filename string, totalChunks int, chunkSize int) {
	timeout := 60 * time.Second
	maxRetries := 5

	for idx := 0; idx < totalChunks; idx++ {
		retries := 0
		for {
			// check if already received
			var received bool
			s.filesMu.Lock()
			for _, fileKey := range []string{addr.String()} {
				if m, ok := s.receivedChunks[fileKey]; ok {
					if m[idx] {
						received = true
					}
				}
			}
			s.filesMu.Unlock()
			if received {
				break
			}

			// send request
			payload := make([]byte, 4+len(filename))
			binary.BigEndian.PutUint32(payload[0:4], uint32(idx))
			copy(payload[4:], []byte(filename))
			s.packetGenerator(addr, RequestChunk, payload, 0, nil)

			// wait on waitChans
			s.waitMux.Lock()
			if s.waitChans[filename] == nil {
				s.waitChans[filename] = make(map[int]chan struct{})
			}
			if s.waitChans[filename][idx] == nil {
				s.waitChans[filename][idx] = make(chan struct{})
			}
			ch := s.waitChans[filename][idx]
			s.waitMux.Unlock()

			select {
			case <-ch:
				// got it
				break
			case <-time.After(timeout):
				retries++
				if retries >= maxRetries {
					fmt.Printf("Request for chunk %d from %s timed out after %d retries\n", idx, addr.String(), retries)
					break
				}
			}

			// check if received
			s.filesMu.Lock()
			got := false
			key := addr.String()
			if m, ok := s.receivedChunks[key]; ok {
				if m[idx] {
					got = true
				}
			}
			s.filesMu.Unlock()
			if got || retries >= maxRetries {
				break
			}
		}
	}

	// check completion; server does not automatically send transfer_complete: receiver should
}

func (s *Server) MessageFromServerAnyTime() {
	for {
		var send, id, msg string
		_, err := fmt.Scanln(&send, &id, &msg)
		if err != nil {
			fmt.Println("Error reading input:", err)
			continue
		}

		client := s.getClientById(id)
		if client == nil {
			fmt.Printf("Client %s not found\n", id)
			continue
		}

		if send == "send" {
			s.packetGenerator(client.Addr, Message, []byte(msg), 0, nil)
		} else if send == "sendfile" {
			err := s.SendFileToClient(client, msg, filepath.Base(msg))
			if err != nil {
				fmt.Println("SendFile error:", err)
			}
		}
	}
}

func (s *Server) Start() {
	go s.MutexHandleActions()
	go s.MutexHandleClientActions()

	for i := 0; i < 1; i++ {
		go s.udpWriteWorker(i)
		go s.pktGWorker()
		go s.packetSender()
		go s.packetParserWorker()
	}

	go s.udpReadWorker()
	go s.fieldPacketTrackingWorker()
	go s.MessageFromServerAnyTime()

	select {}
}

func main() {
	s, err := NewServer(":10000")
	if err != nil {
		panic(err)
	}

	fmt.Println("Server running on port 10000...... :)")
	s.Start()
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
