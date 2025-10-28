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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
	RequestStatus    = 10
	InProgress       = 11
	AlreadySent      = 12
	NotReceived      = 13
	ChunkSize        = 60000 //1200
	UUIDLen          = 36
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
type StateCommand struct {
	Action   string
	Addr     *net.UDPAddr
	Id       string
	Packet   []byte
	PacketID uint16
	Reply    chan any
	AckChan  chan struct{}
}
type FileCommand struct {
	Action string
	Key    string
	Meta   FileMeta
	File   *os.File
	Addr   *net.UDPAddr
	Idx    int
	Reply  chan any
}
type ServeCommand struct {
	Action string
	Key    string
	Meta   FileMeta
	File   *os.File
	Idx    int
	Reply  chan any
}
type WaitCommand struct {
	Action string
	Key    string
	Idx    int
	Status byte
	Reply  chan any
}
type Server struct {
	conn            *net.UDPConn
	clientsByID     map[string]*Client
	clientsByAddr   map[string]*Client
	writeQueue      chan Job
	pendingPackets  map[uint16]PendingPacketsJob
	parseQueue      chan Job
	genQueue        chan GenTask
	builtpackets    chan Job
	stateChan       chan StateCommand
	metaPendingMap  map[uint16]chan struct{}
	snapshot        atomic.Value
	packetIDCounter uint32
	//
	files          map[string]*os.File // incoming files from clients (receiving), key=uuid
	meta           map[string]FileMeta
	receivedChunks map[string]map[int]bool
	addrs          map[string]*net.UDPAddr // uuid -> sender addr
	incomingQueue  chan string             // uuids for processing
	// files to serve (when server acts as sender), key=uuid
	serveFiles     map[string]*os.File
	serveMeta      map[string]FileMeta
	serveRequested map[string]map[int]bool
	serveSent      map[string]map[int]bool
	// waiters for received chunks per uuid per client address
	waitChans      map[string]map[int]chan struct{}
	statusChans    map[string]map[int]chan byte
	fileStateChan  chan FileCommand
	serveStateChan chan ServeCommand
	waitStateChan  chan WaitCommand
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
		stateChan:      make(chan StateCommand, 5000),
		metaPendingMap: make(map[uint16]chan struct{}),
		files:          make(map[string]*os.File),
		meta:           make(map[string]FileMeta),
		receivedChunks: make(map[string]map[int]bool),
		addrs:          make(map[string]*net.UDPAddr),
		incomingQueue:  make(chan string, 100),
		serveFiles:     make(map[string]*os.File),
		serveMeta:      make(map[string]FileMeta),
		serveRequested: make(map[string]map[int]bool),
		serveSent:      make(map[string]map[int]bool),
		waitChans:      make(map[string]map[int]chan struct{}),
		statusChans:    make(map[string]map[int]chan byte),
		fileStateChan:  make(chan FileCommand, 100),
		serveStateChan: make(chan ServeCommand, 100),
		waitStateChan:  make(chan WaitCommand, 100),
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
	s.stateChan <- StateCommand{Action: "registration", Addr: addr, Id: id}
	s.packetGenerator(addr, Ack, []byte("Registered success"), clientAckPacketId, nil)
	fmt.Println("Registered client:", id, addr)
}
func (s *Server) getClientByAddr(addr *net.UDPAddr) *Client {
	reply := make(chan any)
	s.stateChan <- StateCommand{Action: "clientByAddr", Addr: addr, Reply: reply}
	client := (<-reply).(*Client)
	return client
}
func (s *Server) getClientById(id string) *Client {
	reply := make(chan any)
	s.stateChan <- StateCommand{Action: "clientByID", Id: id, Reply: reply}
	client := (<-reply).(*Client)
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
		if task.MsgType != Ack && task.MsgType != RequestChunk && task.MsgType != PendingChunk && task.MsgType != TransferComplete && task.MsgType != RequestStatus && task.MsgType != InProgress && task.MsgType != AlreadySent && task.MsgType != NotReceived {
			binary.BigEndian.PutUint16(packet[0:2], packetID)
			s.stateChan <- StateCommand{Action: "addPending", PacketID: packetID, Addr: task.Addr, Packet: packet}
			if task.AckChan != nil {
				s.stateChan <- StateCommand{Action: "registerAckMetadata", PacketID: packetID, AckChan: task.AckChan}
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
		fmt.Println("Received pending from", addr, string(payload))
	case TransferComplete:
		s.handleTransferComplete(addr, payload, packetID)
	case RequestStatus:
		s.handleRequestStatus(addr, payload, packetID)
	case InProgress, AlreadySent, NotReceived:
		s.handleStatusResponse(addr, msgType, payload, packetID)
	}
}
func (s *Server) handleMetadata(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	parts := strings.Split(string(payload), "|")
	if len(parts) != 4 {
		fmt.Println("Invalid metadata from", addr.String())
		return
	}
	uuid := parts[0]
	filename := parts[1]
	totalChunks, _ := strconv.Atoi(parts[2])
	chunkSz, _ := strconv.Atoi(parts[3])
	fpath := "fromClient_" + filename
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	replyCh := make(chan any)
	s.fileStateChan <- FileCommand{Action: "addIfNotExists", Key: uuid, File: f, Meta: FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: chunkSz}, Addr: addr, Reply: replyCh}
	added := (<-replyCh).(bool)
	if !added {
		f.Close()
		fmt.Printf("Duplicate metadata ignored from %s\n", addr.String())
		return
	}
	s.packetGenerator(addr, Ack, []byte("metadata received"), clientAckPacketId, nil)
	fmt.Printf("Metadata received from %s: %s (%d chunks, %d bytes each)\n", addr.String(), filename, totalChunks, chunkSz)
	s.incomingQueue <- uuid
}
func (s *Server) handleChunk(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	data := make([]byte, len(payload)-4-UUIDLen)
	copy(data, payload[4+UUIDLen:])
	replyCh := make(chan any)
	s.fileStateChan <- FileCommand{Action: "checkAndSetReceived", Key: uuid, Idx: idx, Reply: replyCh}
	isNew := (<-replyCh).(bool)
	if !isNew {
		fmt.Printf("Duplicate chunk %d ignored from %s or metadata not received\n", idx, addr.String())
		s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d already received or metadata missing", idx)), clientAckPacketId, nil)
		return
	}
	replyChFile := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getFile", Key: uuid, Reply: replyChFile}
	f := (<-replyChFile).(*os.File)
	if f == nil {
		fmt.Println("No file handle for", uuid)
		s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d ignored: no file handle", idx)), clientAckPacketId, nil)
		return
	}
	replyChMeta := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getMeta", Key: uuid, Reply: replyChMeta}
	meta := (<-replyChMeta).(FileMeta)
	if meta.Filename == "" {
		fmt.Println("No metadata for", uuid)
		s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d ignored: no metadata", idx)), clientAckPacketId, nil)
		return
	}
	offset := int64(idx * meta.ChunkSize)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}
	s.waitStateChan <- WaitCommand{Action: "notify", Key: uuid, Idx: idx}
	s.packetGenerator(addr, Ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil)
	fmt.Printf("Chunk %d received from %s (%d/%d)\n", idx, addr.String(), meta.Received, meta.TotalChunks)
	replyChRec := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getReceived", Key: uuid, Reply: replyChRec}
	received := (<-replyChRec).(int)
	replyChTot := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getTotal", Key: uuid, Reply: replyChTot}
	total := (<-replyChTot).(int)
	if received >= total && total > 0 {
		s.fileStateChan <- FileCommand{Action: "closeAndDelete", Key: uuid}
		fmt.Printf("File saved from %s: fromClient_%s\n", addr.String(), meta.Filename)
		s.packetGenerator(addr, TransferComplete, []byte(uuid), 0, nil)
		s.waitStateChan <- WaitCommand{Action: "clear", Key: uuid}
		s.waitStateChan <- WaitCommand{Action: "clearStatus", Key: uuid}
	}
}
func (s *Server) fieldPacketTrackingWorker() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		reply := make(chan any)
		s.stateChan <- StateCommand{Action: "getAllPending", Reply: reply}
		pendings := (<-reply).(map[uint16]PendingPacketsJob)
		for packetID, pending := range pendings {
			if now.Sub(pending.LastSend) >= 1*time.Second {
				s.builtpackets <- pending.Job
				s.stateChan <- StateCommand{Action: "updatePending", PacketID: packetID}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
func (s *Server) handleAck(packetID uint16, payload []byte) {
	fmt.Println("Client ack:", string(payload))
	s.stateChan <- StateCommand{Action: "deletePending", PacketID: packetID}
}
func (s *Server) SendFileToClient(client *Client, filepath string, filename string) error {
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
	uuid := uuid.New().String()
	metadataStr := fmt.Sprintf("%s|%s|%d|%d", uuid, filename, totalChunks, ChunkSize)
	s.serveStateChan <- ServeCommand{Action: "addServe", Key: uuid, File: f, Meta: FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: ChunkSize}}
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
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	replyCh := make(chan any)
	s.serveStateChan <- ServeCommand{Action: "getServe", Key: uuid, Reply: replyCh}
	res := (<-replyCh).(struct {
		F  *os.File
		M  FileMeta
		Ok bool
	})
	if !res.Ok {
		fmt.Printf("Request for unknown file %s\n", uuid)
		s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil)
		return
	}
	s.serveStateChan <- ServeCommand{Action: "setRequested", Key: uuid, Idx: idx}
	offset := int64(idx * res.M.ChunkSize)
	buf := make([]byte, res.M.ChunkSize)
	_, err := res.F.ReadAt(buf, offset)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			stat, stErr := res.F.Stat()
			if stErr == nil {
				fileSize := stat.Size()
				start := offset
				if start >= fileSize {
					s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil)
					return
				}
				end := start + int64(res.M.ChunkSize)
				if end > fileSize {
					end = fileSize
				}
				buf = buf[:end-start]
			} else {
				s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil)
				return
			}
		} else {
			s.packetGenerator(addr, PendingChunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil)
			return
		}
	}
	payloadChunk := make([]byte, 4+UUIDLen+len(buf))
	binary.BigEndian.PutUint32(payloadChunk[0:4], uint32(idx))
	copy(payloadChunk[4:4+UUIDLen], []byte(uuid))
	copy(payloadChunk[4+UUIDLen:], buf)
	s.packetGenerator(addr, Chunk, payloadChunk, 0, nil)
	s.serveStateChan <- ServeCommand{Action: "setSent", Key: uuid, Idx: idx}
}
func (s *Server) handleRequestStatus(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	replyCh := make(chan any)
	s.serveStateChan <- ServeCommand{Action: "getServe", Key: uuid, Reply: replyCh}
	res := (<-replyCh).(struct {
		F  *os.File
		M  FileMeta
		Ok bool
	})
	if !res.Ok {
		s.packetGenerator(addr, NotReceived, payload, clientAckPacketId, nil)
		return
	}
	replyReq := make(chan any)
	s.serveStateChan <- ServeCommand{Action: "getRequested", Key: uuid, Idx: idx, Reply: replyReq}
	isReq := (<-replyReq).(bool)
	if !isReq {
		s.packetGenerator(addr, NotReceived, payload, clientAckPacketId, nil)
		return
	}
	replySent := make(chan any)
	s.serveStateChan <- ServeCommand{Action: "getSent", Key: uuid, Idx: idx, Reply: replySent}
	isSent := (<-replySent).(bool)
	if !isSent {
		s.packetGenerator(addr, InProgress, payload, clientAckPacketId, nil)
		return
	}
	s.packetGenerator(addr, AlreadySent, payload, clientAckPacketId, nil)
}
func (s *Server) handleStatusResponse(addr *net.UDPAddr, msgType byte, payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	s.waitStateChan <- WaitCommand{Action: "notifyStatus", Key: uuid, Idx: idx, Status: msgType}
}
func (s *Server) handleTransferComplete(addr *net.UDPAddr, payload []byte, clientAckPacketId uint16) {
	uuid := string(payload)
	replyMeta := make(chan any)
	s.serveStateChan <- ServeCommand{Action: "getMeta", Key: uuid, Reply: replyMeta}
	meta := (<-replyMeta).(FileMeta)
	s.serveStateChan <- ServeCommand{Action: "closeAndDeleteServe", Key: uuid}
	s.waitStateChan <- WaitCommand{Action: "clear", Key: uuid}
	s.waitStateChan <- WaitCommand{Action: "clearStatus", Key: uuid}
	fmt.Printf("Peer %s reports transfer complete for %s\n", addr.String(), meta.Filename)
}
func (s *Server) StateHandler() {
	for {
		select {
		case cmd := <-s.stateChan:
			switch cmd.Action {
			case "registration":
				client := &Client{ID: cmd.Id, Addr: cmd.Addr}
				s.clientsByID[cmd.Id] = client
				s.clientsByAddr[cmd.Addr.String()] = client
			case "clientByAddr":
				cmd.Reply <- s.clientsByAddr[cmd.Addr.String()]
			case "clientByID":
				cmd.Reply <- s.clientsByID[cmd.Id]
			case "registerAckMetadata":
				if cmd.AckChan != nil {
					s.metaPendingMap[cmd.PacketID] = cmd.AckChan
				}
			case "addPending":
				s.pendingPackets[cmd.PacketID] = PendingPacketsJob{
					Job:      Job{Addr: cmd.Addr, Packet: cmd.Packet},
					LastSend: time.Now(),
				}
				s.updatePendingSnapshot()
			case "updatePending":
				if p, ok := s.pendingPackets[cmd.PacketID]; ok {
					p.LastSend = time.Now()
					s.pendingPackets[cmd.PacketID] = p
					s.updatePendingSnapshot()
				}
			case "deletePending":
				delete(s.pendingPackets, cmd.PacketID)
				if ch, ok := s.metaPendingMap[cmd.PacketID]; ok {
					close(ch)
					delete(s.metaPendingMap, cmd.PacketID)
				}
				s.updatePendingSnapshot()
			case "getAllPending":
				snap := s.snapshot.Load().(map[uint16]PendingPacketsJob)
				cmd.Reply <- snap
			}
		}
	}
}
func (s *Server) FileStateHandler() {
	for {
		select {
		case cmd := <-s.fileStateChan:
			switch cmd.Action {
			case "addIfNotExists":
				if _, ok := s.files[cmd.Key]; ok {
					cmd.Reply <- false
				} else {
					s.files[cmd.Key] = cmd.File
					s.meta[cmd.Key] = cmd.Meta
					s.receivedChunks[cmd.Key] = make(map[int]bool)
					s.addrs[cmd.Key] = cmd.Addr
					cmd.Reply <- true
				}
			case "checkAndSetReceived":
				if _, ok := s.receivedChunks[cmd.Key]; !ok {
					cmd.Reply <- false
					continue
				}
				if s.receivedChunks[cmd.Key][cmd.Idx] {
					cmd.Reply <- false
				} else {
					s.receivedChunks[cmd.Key][cmd.Idx] = true
					m := s.meta[cmd.Key]
					m.Received++
					s.meta[cmd.Key] = m
					cmd.Reply <- true
				}
			case "getFile":
				cmd.Reply <- s.files[cmd.Key]
			case "getMeta":
				if m, ok := s.meta[cmd.Key]; ok {
					cmd.Reply <- m
				} else {
					cmd.Reply <- FileMeta{}
				}
			case "getReceived":
				if m, ok := s.meta[cmd.Key]; ok {
					cmd.Reply <- m.Received
				} else {
					cmd.Reply <- 0
				}
			case "getTotal":
				if m, ok := s.meta[cmd.Key]; ok {
					cmd.Reply <- m.TotalChunks
				} else {
					cmd.Reply <- 0
				}
			case "closeAndDelete":
				if f, ok := s.files[cmd.Key]; ok {
					f.Close()
				}
				delete(s.files, cmd.Key)
				delete(s.meta, cmd.Key)
				delete(s.receivedChunks, cmd.Key)
				delete(s.addrs, cmd.Key)
			case "isReceived":
				if m, ok := s.receivedChunks[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "getAddr":
				cmd.Reply <- s.addrs[cmd.Key]
			}
		}
	}
}
func (s *Server) ServeStateHandler() {
	for {
		select {
		case cmd := <-s.serveStateChan:
			switch cmd.Action {
			case "addServe":
				s.serveFiles[cmd.Key] = cmd.File
				s.serveMeta[cmd.Key] = cmd.Meta
				if s.serveRequested[cmd.Key] == nil {
					s.serveRequested[cmd.Key] = make(map[int]bool)
				}
				if s.serveSent[cmd.Key] == nil {
					s.serveSent[cmd.Key] = make(map[int]bool)
				}
			case "getServe":
				f, okf := s.serveFiles[cmd.Key]
				m, okm := s.serveMeta[cmd.Key]
				ok := okf && okm
				cmd.Reply <- struct {
					F  *os.File
					M  FileMeta
					Ok bool
				}{F: f, M: m, Ok: ok}
			case "setRequested":
				if m, ok := s.serveRequested[cmd.Key]; ok {
					m[cmd.Idx] = true
				}
			case "setSent":
				if m, ok := s.serveSent[cmd.Key]; ok {
					m[cmd.Idx] = true
				}
			case "getRequested":
				if m, ok := s.serveRequested[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "getSent":
				if m, ok := s.serveSent[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "closeAndDeleteServe":
				if f, ok := s.serveFiles[cmd.Key]; ok {
					f.Close()
				}
				delete(s.serveFiles, cmd.Key)
				delete(s.serveMeta, cmd.Key)
				delete(s.serveRequested, cmd.Key)
				delete(s.serveSent, cmd.Key)
			case "getMeta":
				if m, ok := s.serveMeta[cmd.Key]; ok {
					cmd.Reply <- m
				} else {
					cmd.Reply <- FileMeta{}
				}
			}
		}
	}
}
func (s *Server) WaitStateHandler() {
	for {
		select {
		case cmd := <-s.waitStateChan:
			switch cmd.Action {
			case "ensureChan":
				if _, ok := s.waitChans[cmd.Key]; !ok {
					s.waitChans[cmd.Key] = make(map[int]chan struct{})
				}
				if _, ok := s.waitChans[cmd.Key][cmd.Idx]; !ok {
					s.waitChans[cmd.Key][cmd.Idx] = make(chan struct{})
				}
				cmd.Reply <- s.waitChans[cmd.Key][cmd.Idx]
			case "notify":
				if chmap, ok := s.waitChans[cmd.Key]; ok {
					if ch, ok2 := chmap[cmd.Idx]; ok2 {
						select {
						case <-ch:
						default:
							close(ch)
						}
						delete(s.waitChans[cmd.Key], cmd.Idx)
					}
				}
			case "ensureStatusChan":
				if _, ok := s.statusChans[cmd.Key]; !ok {
					s.statusChans[cmd.Key] = make(map[int]chan byte)
				}
				if _, ok := s.statusChans[cmd.Key][cmd.Idx]; !ok {
					s.statusChans[cmd.Key][cmd.Idx] = make(chan byte)
				}
				cmd.Reply <- s.statusChans[cmd.Key][cmd.Idx]
			case "notifyStatus":
				if chmap, ok := s.statusChans[cmd.Key]; ok {
					if ch, ok2 := chmap[cmd.Idx]; ok2 {
						select {
						case <-ch:
						default:
							ch <- cmd.Status
						}
						delete(s.statusChans[cmd.Key], cmd.Idx)
					}
				}
			case "clear":
				if chmap, ok := s.waitChans[cmd.Key]; ok {
					for _, ch := range chmap {
						close(ch)
					}
					delete(s.waitChans, cmd.Key)
				}
			case "clearStatus":
				if chmap, ok := s.statusChans[cmd.Key]; ok {
					for _, ch := range chmap {
						close(ch)
					}
					delete(s.statusChans, cmd.Key)
				}
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
func (s *Server) requestManagerForIncoming(addr *net.UDPAddr, uuid string, totalChunks int, chunkSize int) {
	timeout := 60 * time.Second
	maxRetries := 5
	for idx := 0; idx < totalChunks; idx++ {
		retries := 0
		for {
			replyCh := make(chan any)
			s.fileStateChan <- FileCommand{Action: "isReceived", Key: uuid, Idx: idx, Reply: replyCh}
			received := (<-replyCh).(bool)
			if received {
				break
			}
			payload := make([]byte, 4+UUIDLen)
			binary.BigEndian.PutUint32(payload[0:4], uint32(idx))
			copy(payload[4:], []byte(uuid))
			s.packetGenerator(addr, RequestChunk, payload, 0, nil)
			replyW := make(chan any)
			s.waitStateChan <- WaitCommand{Action: "ensureChan", Key: uuid, Idx: idx, Reply: replyW}
			ch := (<-replyW).(chan struct{})
			select {
			case <-ch:
				break
			case <-time.After(timeout):
				// Send status request instead of immediate retry
				replyS := make(chan any)
				s.waitStateChan <- WaitCommand{Action: "ensureStatusChan", Key: uuid, Idx: idx, Reply: replyS}
				stCh := (<-replyS).(chan byte)
				payloadS := make([]byte, 4+UUIDLen)
				binary.BigEndian.PutUint32(payloadS[0:4], uint32(idx))
				copy(payloadS[4:], []byte(uuid))
				s.packetGenerator(addr, RequestStatus, payloadS, 0, nil)
				select {
				case status := <-stCh:
					if status == InProgress {
						// Wait more, reset timeout
						continue
					} else {
						// AlreadySent or NotReceived: retry request
						retries++
					}
				case <-time.After(10 * time.Second):
					retries++
				}
				if retries >= maxRetries {
					fmt.Printf("Request for chunk %d from %s timed out after %d retries\n", idx, addr.String(), retries)
					break
				}
			}
			replyCh2 := make(chan any)
			s.fileStateChan <- FileCommand{Action: "isReceived", Key: uuid, Idx: idx, Reply: replyCh2}
			got := (<-replyCh2).(bool)
			if got || retries >= maxRetries {
				break
			}
		}
	}
	replyChRec := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getReceived", Key: uuid, Reply: replyChRec}
	received := (<-replyChRec).(int)
	replyChTot := make(chan any)
	s.fileStateChan <- FileCommand{Action: "getTotal", Key: uuid, Reply: replyChTot}
	total := (<-replyChTot).(int)
	if received+1 >= total && total > 0 {
		s.packetGenerator(addr, TransferComplete, []byte(uuid), 0, nil)
	}
}
func (s *Server) incomingWorker() {
	for uuid := range s.incomingQueue {
		replyMeta := make(chan any)
		s.fileStateChan <- FileCommand{Action: "getMeta", Key: uuid, Reply: replyMeta}
		meta := (<-replyMeta).(FileMeta)
		replyAddr := make(chan any)
		s.fileStateChan <- FileCommand{Action: "getAddr", Key: uuid, Reply: replyAddr}
		addr := (<-replyAddr).(*net.UDPAddr)
		s.requestManagerForIncoming(addr, uuid, meta.TotalChunks, meta.ChunkSize)
	}
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
	go s.StateHandler()
	go s.FileStateHandler()
	go s.ServeStateHandler()
	go s.WaitStateHandler()
	for i := 0; i < 10; i++ {
		go s.incomingWorker()
	}
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
