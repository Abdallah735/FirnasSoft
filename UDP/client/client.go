// === client.go (modified with BATCH REQUEST + SLIDING WINDOW) ===
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
	_register            = 1
	_ping                = 2
	_message             = 3
	_ack                 = 4
	_metadata            = 5
	_chunk               = 6
	_request_chunk       = 7
	_pending_chunk       = 8
	_transfer_complete   = 9
	_request_status      = 10
	_in_progress         = 11
	_already_sent        = 12
	_not_received        = 13
	_batch_request_chunk = 14
	ChunkSize            = 60000
	UUIDLen              = 36
	WindowSize           = 20
)

type Job struct {
	Addr   *net.UDPAddr
	Packet []byte
}

type GenTask struct {
	MsgType           byte
	Payload           []byte
	ClientAckPacketId uint16
	AckChan           chan struct{}
	RespChan          chan uint16
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

type PendingPacketsJob struct {
	Job
	LastSend time.Time
}

type Client struct {
	id              string
	serverAddr      *net.UDPAddr
	conn            *net.UDPConn
	writeQueue      chan Job
	parseQueue      chan Job
	genQueue        chan GenTask
	pendingPackets  map[uint16]PendingPacketsJob
	stateChan       chan StateCommand
	snapshot        atomic.Value
	packetIDCounter uint32
	rttEstimate     atomic.Value
	ackPendingMap   map[uint16]chan struct{}
	//
	files          map[string]*os.File
	meta           map[string]FileMeta
	receivedChunks map[string]map[int]bool
	receivedCount  map[string]int
	//
	serveFiles     map[string]*os.File
	serveMeta      map[string]FileMeta
	serveRequested map[string]map[int]bool
	serveSent      map[string]map[int]bool
	//
	waitChans          map[string]map[int]chan struct{}
	statusChans        map[string]map[int]chan byte
	receivingStateChan chan ReceivingCommand
	serveStateChan     chan ServeCommand
	waitStateChan      chan WaitCommand
	receivingQueue     chan string
}

type ReceivingCommand struct {
	Action      string
	Key         string
	Filename    string
	TotalChunks int
	ChunkSize   int
	File        *os.File
	Idx         int
	Reply       chan any
}

type FileMeta struct {
	Filename    string
	TotalChunks int
	ChunkSize   int
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

func NewClient(id string, server string) *Client {
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		panic(err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		panic(err)
	}
	c := &Client{
		id:                 id,
		serverAddr:         addr,
		conn:               conn,
		writeQueue:         make(chan Job, 5000),
		parseQueue:         make(chan Job, 5000),
		genQueue:           make(chan GenTask, 5000),
		pendingPackets:     make(map[uint16]PendingPacketsJob),
		stateChan:          make(chan StateCommand, 5000),
		ackPendingMap:      make(map[uint16]chan struct{}),
		files:              make(map[string]*os.File),
		meta:               make(map[string]FileMeta),
		receivedChunks:     make(map[string]map[int]bool),
		receivedCount:      make(map[string]int),
		serveFiles:         make(map[string]*os.File),
		serveMeta:          make(map[string]FileMeta),
		serveRequested:     make(map[string]map[int]bool),
		serveSent:          make(map[string]map[int]bool),
		waitChans:          make(map[string]map[int]chan struct{}),
		statusChans:        make(map[string]map[int]chan byte),
		receivingStateChan: make(chan ReceivingCommand, 100),
		serveStateChan:     make(chan ServeCommand, 100),
		waitStateChan:      make(chan WaitCommand, 100),
		receivingQueue:     make(chan string, 100),
	}
	c.packetIDCounter = 0
	c.rttEstimate.Store(500 * time.Millisecond)
	c.snapshot.Store(make(map[uint16]PendingPacketsJob))
	return c
}

func (c *Client) writeWorker(id int) {
	for {
		job := <-c.writeQueue
		_, err := c.conn.Write(job.Packet)
		if err != nil {
			fmt.Printf("Writer %d error: %v\n", id, err)
		}
	}
}

func (c *Client) readWorker() {
	for {
		buffer := make([]byte, 65507)
		n, _, err := c.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error receiving:", err)
			continue
		}
		packet := make([]byte, n)
		copy(packet, buffer[:n])
		c.parseQueue <- Job{Addr: c.serverAddr, Packet: packet}
	}
}

func (c *Client) fieldPacketTrackingWorker() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		reply := make(chan any)
		c.stateChan <- StateCommand{Action: "getAllPending", Reply: reply}
		pendings := (<-reply).(map[uint16]PendingPacketsJob)
		for packetID, pending := range pendings {
			rtt := c.rttEstimate.Load().(time.Duration)
			timeout := rtt * 2
			if timeout < 800*time.Millisecond {
				timeout = 800 * time.Millisecond
			}
			if now.Sub(pending.LastSend) >= timeout {
				c.writeQueue <- pending.Job
				c.stateChan <- StateCommand{Action: "updatePending", PacketID: packetID}
			}
		}
	}
}

func (c *Client) packetParserWorker() {
	for {
		job := <-c.parseQueue
		c.PacketParser(job.Packet)
	}
}

func (c *Client) packetGenerator(msgType byte, payload []byte, clientAckPacketId uint16, ackChan chan struct{}, resp chan uint16) {
	task := GenTask{
		MsgType:           msgType,
		Payload:           payload,
		ClientAckPacketId: clientAckPacketId,
		AckChan:           ackChan,
		RespChan:          resp,
	}
	c.genQueue <- task
}

func (c *Client) packetGeneratorWorker() {
	for {
		task := <-c.genQueue
		packet := make([]byte, 2+2+1+len(task.Payload))
		pid := atomic.AddUint32(&c.packetIDCounter, 1)
		packetID := uint16(pid & 0xFFFF)
		binary.BigEndian.PutUint16(packet[2:4], 0)
		packet[4] = task.MsgType
		copy(packet[5:], task.Payload)
		if task.MsgType != _ack && task.MsgType != _request_chunk && task.MsgType != _pending_chunk && task.MsgType != _transfer_complete && task.MsgType != _request_status && task.MsgType != _in_progress && task.MsgType != _already_sent && task.MsgType != _not_received && task.MsgType != _batch_request_chunk {
			binary.BigEndian.PutUint16(packet[0:2], packetID)
			c.stateChan <- StateCommand{Action: "addPending", PacketID: packetID, Packet: packet}
			if task.AckChan != nil {
				c.stateChan <- StateCommand{Action: "registerAck", PacketID: packetID, AckChan: task.AckChan}
			}
			if task.RespChan != nil {
				task.RespChan <- packetID
			}
		} else {
			binary.BigEndian.PutUint16(packet[0:2], task.ClientAckPacketId)
		}
		c.writeQueue <- Job{Addr: c.serverAddr, Packet: packet}
	}
}

func (c *Client) PacketParser(packet []byte) {
	if len(packet) < 5 {
		return
	}
	packetID := binary.BigEndian.Uint16(packet[0:2])
	msgType := packet[4]
	payload := packet[5:]
	switch msgType {
	case _message:
		c.packetGenerator(_ack, []byte("message received"), packetID, nil, nil)
		fmt.Println("Server:", string(payload))
	case _ack:
		fmt.Println("Peer ack:", string(payload))
		c.stateChan <- StateCommand{Action: "deletePending", PacketID: packetID}
	case _metadata:
		c.handleMetadata(payload, packetID)
	case _chunk:
		c.handleChunk(payload, packetID)
	case _request_chunk:
		c.handleRequestChunk(payload, packetID)
	case _pending_chunk:
		c.handlePendingChunk(payload, packetID)
	case _transfer_complete:
		c.handleTransferComplete(payload, packetID)
	case _request_status:
		c.handleRequestStatus(payload, packetID)
	case _in_progress, _already_sent, _not_received:
		c.handleStatusResponse(msgType, payload, packetID)
	case _batch_request_chunk:
		c.handleBatchRequestChunk(payload, packetID)
	}
}

func (c *Client) Register() {
	c.packetGenerator(_register, []byte(c.id), 0, nil, nil)
	fmt.Println("register:", c.id)
}

func (c *Client) Ping() {
	c.packetGenerator(_ping, []byte("ping"), 0, nil, nil)
	fmt.Println("ping")
}

func (c *Client) SendMessage(message string) {
	c.packetGenerator(_message, []byte(message), 0, nil, nil)
}

func (c *Client) handleMetadata(payload []byte, clientAckPacketId uint16) {
	meta := string(payload)
	parts := strings.Split(meta, "|")
	if len(parts) != 4 {
		fmt.Println("Invalid metadata format")
		return
	}
	uuid := parts[0]
	filename := parts[1]
	totalChunks, _ := strconv.Atoi(parts[2])
	chunkSize, _ := strconv.Atoi(parts[3])
	fpath := "fromPeer_" + filename
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	replyCh := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "addIfNotExists", Key: uuid, Filename: filename, TotalChunks: totalChunks, ChunkSize: chunkSize, File: f, Reply: replyCh}
	_ = (<-replyCh).(bool)
	c.packetGenerator(_ack, []byte("metadata received"), clientAckPacketId, nil, nil)
	fmt.Printf("Metadata received: %s (%d chunks, %d bytes each)\n", filename, totalChunks, chunkSize)
	c.receivingQueue <- uuid
}

func (c *Client) handleChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	data := make([]byte, len(payload)-4-UUIDLen)
	copy(data, payload[4+UUIDLen:])

	replyIs := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Key: uuid, Idx: idx, Reply: replyIs}
	isDup := (<-replyIs).(bool)
	if isDup {
		c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d already received", idx)), clientAckPacketId, nil, nil)
		return
	}

	c.receivingStateChan <- ReceivingCommand{Action: "setReceived", Key: uuid, Idx: idx}
	replyFile := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getFile", Key: uuid, Reply: replyFile}
	f := (<-replyFile).(*os.File)
	replySize := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getChunkSize", Key: uuid, Reply: replySize}
	size := (<-replySize).(int)
	replyName := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getFilename", Key: uuid, Reply: replyName}
	filename := (<-replyName).(string)
	offset := int64(idx * size)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}
	c.waitStateChan <- WaitCommand{Action: "notify", Key: uuid, Idx: idx}
	c.receivingStateChan <- ReceivingCommand{Action: "incrementCount", Key: uuid}
	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil, nil)
	fmt.Printf("Chunk %d received (%s)\n", idx, filename)
}

func (c *Client) handleBatchRequestChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 8+UUIDLen {
		return
	}
	start := int(binary.BigEndian.Uint32(payload[0:4]))
	end := int(binary.BigEndian.Uint32(payload[4:8]))
	uuid := string(payload[8 : 8+UUIDLen])

	replyCh := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getServe", Key: uuid, Reply: replyCh}
	res := (<-replyCh).(struct {
		F  *os.File
		M  FileMeta
		Ok bool
	})
	if !res.Ok {
		return
	}

	for idx := start; idx < end; idx++ {
		replySent := make(chan any)
		c.serveStateChan <- ServeCommand{Action: "getSent", Key: uuid, Idx: idx, Reply: replySent}
		if (<-replySent).(bool) {
			continue
		}

		offset := int64(idx * res.M.ChunkSize)
		buf := make([]byte, res.M.ChunkSize)
		n, err := res.F.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			continue
		}
		if n == 0 {
			continue
		}
		if err == io.EOF {
			buf = buf[:n]
		}

		chunkPayload := make([]byte, 4+UUIDLen+len(buf))
		binary.BigEndian.PutUint32(chunkPayload[0:4], uint32(idx))
		copy(chunkPayload[4:4+UUIDLen], []byte(uuid))
		copy(chunkPayload[4+UUIDLen:], buf)

		c.packetGenerator(_chunk, chunkPayload, 0, nil, nil)
		c.serveStateChan <- ServeCommand{Action: "setSent", Key: uuid, Idx: idx}
	}
}

func (c *Client) requestManagerForFile(uuid string, totalChunks int, chunkSize int) {
	// Phase 1: Send batch requests
	for start := 0; start < totalChunks; start += WindowSize {
		end := start + WindowSize
		if end > totalChunks {
			end = totalChunks
		}
		payload := make([]byte, 8+UUIDLen)
		binary.BigEndian.PutUint32(payload[0:4], uint32(start))
		binary.BigEndian.PutUint32(payload[4:8], uint32(end))
		copy(payload[8:], []byte(uuid))
		c.packetGenerator(_batch_request_chunk, payload, 0, nil, nil)
	}

	time.Sleep(3 * time.Second)

	// Phase 2: Re-request missing chunks
	missing := []int{}
	reply := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getMissing", Key: uuid, TotalChunks: totalChunks, Reply: reply}
	missing = (<-reply).([]int)

	for len(missing) > 0 {
		for i := 0; i < len(missing); i += WindowSize {
			end := i + WindowSize
			if end > len(missing) {
				end = len(missing)
			}
			startIdx := missing[i]
			endIdx := missing[end-1]
			payload := make([]byte, 8+UUIDLen)
			binary.BigEndian.PutUint32(payload[0:4], uint32(startIdx))
			binary.BigEndian.PutUint32(payload[4:8], uint32(endIdx))
			copy(payload[8:], []byte(uuid))
			c.packetGenerator(_batch_request_chunk, payload, 0, nil, nil)
		}
		time.Sleep(2 * time.Second)
		c.receivingStateChan <- ReceivingCommand{Action: "getMissing", Key: uuid, TotalChunks: totalChunks, Reply: reply}
		missing = (<-reply).([]int)
	}

	// Final check
	replyCount := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getCount", Key: uuid, Reply: replyCount}
	count := (<-replyCount).(int)
	if count >= totalChunks {
		c.packetGenerator(_transfer_complete, []byte(uuid), 0, nil, nil)
		c.receivingStateChan <- ReceivingCommand{Action: "closeFile", Key: uuid}
		fmt.Printf("File fully received: %s\n", uuid)
	} else {
		fmt.Printf("File partially received (%d/%d)\n", count, totalChunks)
	}
}

func (c *Client) SendFileToServer(path string) error {
	f, err := os.Open(path)
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
	filename := filepath.Base(path)
	uuid := uuid.New().String()
	c.serveStateChan <- ServeCommand{Action: "addServe", Key: uuid, File: f, Meta: FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: ChunkSize}}
	metadataStr := fmt.Sprintf("%s|%s|%d|%d", uuid, filename, totalChunks, ChunkSize)
	metaAck := make(chan struct{})
	c.packetGenerator(_metadata, []byte(metadataStr), 0, metaAck, nil)
	select {
	case <-metaAck:
		fmt.Println("Metadata ack received, waiting for batch requests")
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting metadata ack")
	}
	return nil
}

func (c *Client) handleRequestChunk(payload []byte, clientAckPacketId uint16) {
	// fallback for old clients
	if len(payload) >= 4+UUIDLen {
		idx := int(binary.BigEndian.Uint32(payload[0:4]))
		uuid := string(payload[4 : 4+UUIDLen])
		fakeBatch := make([]byte, 8+UUIDLen)
		binary.BigEndian.PutUint32(fakeBatch[0:4], uint32(idx))
		binary.BigEndian.PutUint32(fakeBatch[4:8], uint32(idx+1))
		copy(fakeBatch[8:], []byte(uuid))
		c.handleBatchRequestChunk(fakeBatch, clientAckPacketId)
	}
}

func (c *Client) handleRequestStatus(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	replyCh := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getServe", Key: uuid, Reply: replyCh}
	res := (<-replyCh).(struct {
		F  *os.File
		M  FileMeta
		Ok bool
	})
	if !res.Ok {
		c.packetGenerator(_not_received, payload, clientAckPacketId, nil, nil)
		return
	}
	replyReq := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getRequested", Key: uuid, Idx: idx, Reply: replyReq}
	isReq := (<-replyReq).(bool)
	if !isReq {
		c.packetGenerator(_not_received, payload, clientAckPacketId, nil, nil)
		return
	}
	replySent := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getSent", Key: uuid, Idx: idx, Reply: replySent}
	isSent := (<-replySent).(bool)
	if !isSent {
		c.packetGenerator(_in_progress, payload, clientAckPacketId, nil, nil)
		return
	}
	c.packetGenerator(_already_sent, payload, clientAckPacketId, nil, nil)
}

func (c *Client) handleStatusResponse(msgType byte, payload []byte, packetID uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	c.waitStateChan <- WaitCommand{Action: "notifyStatus", Key: uuid, Idx: idx, Status: msgType}
}

func (c *Client) handlePendingChunk(payload []byte, clientAckPacketId uint16) {
	// يمكن تجاهله أو استخدامه لاحقاً
}

func (c *Client) handleTransferComplete(payload []byte, clientAckPacketId uint16) {
	uuid := string(payload)
	replyMeta := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getMeta", Key: uuid, Reply: replyMeta}
	meta := (<-replyMeta).(FileMeta)
	c.serveStateChan <- ServeCommand{Action: "closeAndDeleteServe", Key: uuid}
	c.waitStateChan <- WaitCommand{Action: "clear", Key: uuid}
	c.waitStateChan <- WaitCommand{Action: "clearStatus", Key: uuid}
	fmt.Printf("Server reports transfer complete for %s\n", meta.Filename)
}

func (c *Client) StateHandler() {
	for {
		select {
		case cmd := <-c.stateChan:
			switch cmd.Action {
			case "addPending":
				c.pendingPackets[cmd.PacketID] = PendingPacketsJob{
					Job:      Job{Addr: cmd.Addr, Packet: cmd.Packet},
					LastSend: time.Now(),
				}
				c.updatePendingSnapshot()
			case "updatePending":
				if p, ok := c.pendingPackets[cmd.PacketID]; ok {
					p.LastSend = time.Now()
					c.pendingPackets[cmd.PacketID] = p
					c.updatePendingSnapshot()
				}
			case "deletePending":
				delete(c.pendingPackets, cmd.PacketID)
				if ch, ok := c.ackPendingMap[cmd.PacketID]; ok {
					close(ch)
					delete(c.ackPendingMap, cmd.PacketID)
				}
				c.updatePendingSnapshot()
			case "registerAck":
				if cmd.AckChan != nil {
					c.ackPendingMap[cmd.PacketID] = cmd.AckChan
				}
			case "getAllPending":
				snap := c.snapshot.Load().(map[uint16]PendingPacketsJob)
				cmd.Reply <- snap
			}
		}
	}
}

func (c *Client) ReceivingStateHandler() {
	for {
		select {
		case cmd := <-c.receivingStateChan:
			switch cmd.Action {
			case "addIfNotExists":
				if _, ok := c.files[cmd.Key]; ok {
					cmd.Reply <- false
				} else {
					c.files[cmd.Key] = cmd.File
					c.meta[cmd.Key] = FileMeta{Filename: cmd.Filename, TotalChunks: cmd.TotalChunks, ChunkSize: cmd.ChunkSize}
					c.receivedChunks[cmd.Key] = make(map[int]bool)
					for i := 0; i < cmd.TotalChunks; i++ {
						c.receivedChunks[cmd.Key][i] = false
					}
					c.receivedCount[cmd.Key] = 0
					cmd.Reply <- true
				}
			case "isReceived":
				if m, ok := c.receivedChunks[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "setReceived":
				if m, ok := c.receivedChunks[cmd.Key]; ok {
					m[cmd.Idx] = true
				}
			case "getFile":
				cmd.Reply <- c.files[cmd.Key]
			case "getChunkSize":
				if m, ok := c.meta[cmd.Key]; ok {
					cmd.Reply <- m.ChunkSize
				} else {
					cmd.Reply <- 0
				}
			case "getFilename":
				if m, ok := c.meta[cmd.Key]; ok {
					cmd.Reply <- m.Filename
				} else {
					cmd.Reply <- ""
				}
			case "incrementCount":
				c.receivedCount[cmd.Key]++
			case "getCount":
				cmd.Reply <- c.receivedCount[cmd.Key]
			case "getTotal":
				if m, ok := c.meta[cmd.Key]; ok {
					cmd.Reply <- m.TotalChunks
				} else {
					cmd.Reply <- 0
				}
			case "closeFile":
				if f, ok := c.files[cmd.Key]; ok {
					f.Close()
				}
				delete(c.files, cmd.Key)
				delete(c.meta, cmd.Key)
				delete(c.receivedChunks, cmd.Key)
				delete(c.receivedCount, cmd.Key)
			case "getMissing":
				if m, ok := c.receivedChunks[cmd.Key]; ok {
					missing := []int{}
					for i := 0; i < cmd.TotalChunks; i++ {
						if !m[i] {
							missing = append(missing, i)
						}
					}
					cmd.Reply <- missing
				} else {
					cmd.Reply <- []int{}
				}
			}
		}
	}
}

func (c *Client) ServeStateHandler() {
	for {
		select {
		case cmd := <-c.serveStateChan:
			switch cmd.Action {
			case "addServe":
				c.serveFiles[cmd.Key] = cmd.File
				c.serveMeta[cmd.Key] = cmd.Meta
				if c.serveRequested[cmd.Key] == nil {
					c.serveRequested[cmd.Key] = make(map[int]bool)
				}
				if c.serveSent[cmd.Key] == nil {
					c.serveSent[cmd.Key] = make(map[int]bool)
				}
			case "getServe":
				f, okf := c.serveFiles[cmd.Key]
				m, okm := c.serveMeta[cmd.Key]
				ok := okf && okm
				cmd.Reply <- struct {
					F  *os.File
					M  FileMeta
					Ok bool
				}{F: f, M: m, Ok: ok}
			case "setRequested":
				if m, ok := c.serveRequested[cmd.Key]; ok {
					m[cmd.Idx] = true
				}
			case "setSent":
				if m, ok := c.serveSent[cmd.Key]; ok {
					m[cmd.Idx] = true
				}
			case "getRequested":
				if m, ok := c.serveRequested[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "getSent":
				if m, ok := c.serveSent[cmd.Key]; ok {
					cmd.Reply <- m[cmd.Idx]
				} else {
					cmd.Reply <- false
				}
			case "closeAndDeleteServe":
				if f, ok := c.serveFiles[cmd.Key]; ok {
					f.Close()
				}
				delete(c.serveFiles, cmd.Key)
				delete(c.serveMeta, cmd.Key)
				delete(c.serveRequested, cmd.Key)
				delete(c.serveSent, cmd.Key)
			case "getMeta":
				if m, ok := c.serveMeta[cmd.Key]; ok {
					cmd.Reply <- m
				} else {
					cmd.Reply <- FileMeta{}
				}
			}
		}
	}
}

func (c *Client) WaitStateHandler() {
	for {
		select {
		case cmd := <-c.waitStateChan:
			switch cmd.Action {
			case "ensureChan":
				if _, ok := c.waitChans[cmd.Key]; !ok {
					c.waitChans[cmd.Key] = make(map[int]chan struct{})
				}
				if _, ok := c.waitChans[cmd.Key][cmd.Idx]; !ok {
					c.waitChans[cmd.Key][cmd.Idx] = make(chan struct{})
				}
				cmd.Reply <- c.waitChans[cmd.Key][cmd.Idx]
			case "notify":
				if chmap, ok := c.waitChans[cmd.Key]; ok {
					if ch, ok2 := chmap[cmd.Idx]; ok2 {
						select {
						case <-ch:
						default:
							close(ch)
						}
						delete(c.waitChans[cmd.Key], cmd.Idx)
					}
				}
			case "ensureStatusChan":
				if _, ok := c.statusChans[cmd.Key]; !ok {
					c.statusChans[cmd.Key] = make(map[int]chan byte)
				}
				if _, ok := c.statusChans[cmd.Key][cmd.Idx]; !ok {
					c.statusChans[cmd.Key][cmd.Idx] = make(chan byte)
				}
				cmd.Reply <- c.statusChans[cmd.Key][cmd.Idx]
			case "notifyStatus":
				if chmap, ok := c.statusChans[cmd.Key]; ok {
					if ch, ok2 := chmap[cmd.Idx]; ok2 {
						select {
						case <-ch:
						default:
							ch <- cmd.Status
						}
						delete(c.statusChans[cmd.Key], cmd.Idx)
					}
				}
			case "clear":
				if chmap, ok := c.waitChans[cmd.Key]; ok {
					for _, ch := range chmap {
						close(ch)
					}
					delete(c.waitChans, cmd.Key)
				}
			case "clearStatus":
				if chmap, ok := c.statusChans[cmd.Key]; ok {
					for _, ch := range chmap {
						close(ch)
					}
					delete(c.statusChans, cmd.Key)
				}
			}
		}
	}
}

func (c *Client) updatePendingSnapshot() {
	cp := make(map[uint16]PendingPacketsJob, len(c.pendingPackets))
	for k, v := range c.pendingPackets {
		cp[k] = v
	}
	c.snapshot.Store(cp)
}

func (c *Client) receivingWorker() {
	for uuid := range c.receivingQueue {
		replyMeta := make(chan any)
		c.receivingStateChan <- ReceivingCommand{Action: "getMeta", Key: uuid, Reply: replyMeta}
		meta := (<-replyMeta).(FileMeta)
		c.requestManagerForFile(uuid, meta.TotalChunks, meta.ChunkSize)
	}
}

func (c *Client) Start() {
	go c.StateHandler()
	go c.ReceivingStateHandler()
	go c.ServeStateHandler()
	go c.WaitStateHandler()
	for i := 0; i < 10; i++ {
		go c.receivingWorker()
	}
	for i := 0; i < 5; i++ {
		go c.writeWorker(i)
		go c.packetGeneratorWorker()
	}
	go c.readWorker()
	go c.packetParserWorker()
	go c.fieldPacketTrackingWorker()
}

func main() {
	client := NewClient("2", "173.208.144.109:10000")
	client.Start()
	client.Register()
	ticker := time.NewTicker(28 * time.Second)
	go func() {
		for range ticker.C {
			client.Ping()
		}
	}()
	for {
		var input string
		fmt.Scan(&input)
		if strings.HasPrefix(input, "sendfile:") {
			path := strings.TrimPrefix(input, "sendfile:")
			fmt.Println("sending file:", path)
			if err := client.SendFileToServer(path); err != nil {
				fmt.Println("SendFile error:", err)
			}
			continue
		}
		client.SendMessage(input)
	}
}
