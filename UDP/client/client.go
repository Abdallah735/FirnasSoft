// === client.go (modified) ===
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
	_register          = 1
	_ping              = 2
	_message           = 3
	_ack               = 4
	_metadata          = 5
	_chunk             = 6
	_request_chunk     = 7
	_pending_chunk     = 8
	_transfer_complete = 9
	_request_status    = 10
	_in_progress       = 11
	_already_sent      = 12
	_not_received      = 13
	ChunkSize          = 60000 //1200
	UUIDLen            = 36
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
	// file receiving, key=uuid
	files          map[string]*os.File
	meta           map[string]FileMeta
	receivedChunks map[string]map[int]bool
	receivedCount  map[string]int
	// serving files (when this client acts as sender), key=uuid
	serveFiles     map[string]*os.File
	serveMeta      map[string]FileMeta
	serveRequested map[string]map[int]bool
	serveSent      map[string]map[int]bool
	// per-file chunk waiters (to notify request-manager that chunk arrived)
	waitChans          map[string]map[int]chan struct{}
	statusChans        map[string]map[int]chan byte
	receivingStateChan chan ReceivingCommand
	serveStateChan     chan ServeCommand
	waitStateChan      chan WaitCommand
	receivingQueue     chan string // uuids for processing
}
type ReceivingCommand struct {
	Action      string
	Key         string
	Filename    string // display filename for setMetadata
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
		if task.MsgType != _ack && task.MsgType != _request_chunk && task.MsgType != _pending_chunk && task.MsgType != _transfer_complete && task.MsgType != _request_status && task.MsgType != _in_progress && task.MsgType != _already_sent && task.MsgType != _not_received {
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
		fmt.Printf("Duplicate chunk %d ignored\n", idx)
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
	replyCount := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getCount", Key: uuid, Reply: replyCount}
	count := (<-replyCount).(int)
	replyTotal := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getTotal", Key: uuid, Reply: replyTotal}
	total := (<-replyTotal).(int)
	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil, nil)
	fmt.Printf("Chunk %d received and written (%d/%d)\n", idx, count, total)
	if count >= total {
		c.receivingStateChan <- ReceivingCommand{Action: "closeFile", Key: uuid}
		fmt.Printf("File saved from peer: fromPeer_%s\n", filename)
		c.packetGenerator(_transfer_complete, []byte(uuid), 0, nil, nil)
		c.waitStateChan <- WaitCommand{Action: "clear", Key: uuid}
		c.waitStateChan <- WaitCommand{Action: "clearStatus", Key: uuid}
	}
}
func (c *Client) requestManagerForFile(uuid string, totalChunks int, chunkSize int) {
	timeout := 60 * time.Second
	maxRetries := 5
	for idx := 0; idx < totalChunks; idx++ {
		retries := 0
		for {
			replyIs := make(chan any)
			c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Key: uuid, Idx: idx, Reply: replyIs}
			already := (<-replyIs).(bool)
			if already {
				break
			}
			payload := make([]byte, 4+UUIDLen)
			binary.BigEndian.PutUint32(payload[0:4], uint32(idx))
			copy(payload[4:], []byte(uuid))
			c.packetGenerator(_request_chunk, payload, 0, nil, nil)
			replyW := make(chan any)
			c.waitStateChan <- WaitCommand{Action: "ensureChan", Key: uuid, Idx: idx, Reply: replyW}
			ch := (<-replyW).(chan struct{})
			select {
			case <-ch:
				fmt.Printf("chunk receiving case occured for chunk: %v\n", idx)
				break
			case <-time.After(timeout):
				fmt.Printf("timeout case occred for chunk: %v\n", idx)
				// Send status request
				replyS := make(chan any)
				c.waitStateChan <- WaitCommand{Action: "ensureStatusChan", Key: uuid, Idx: idx, Reply: replyS}
				stCh := (<-replyS).(chan byte)
				payloadS := make([]byte, 4+UUIDLen)
				binary.BigEndian.PutUint32(payloadS[0:4], uint32(idx))
				copy(payloadS[4:], []byte(uuid))
				c.packetGenerator(_request_status, payloadS, 0, nil, nil)
				select {
				case status := <-stCh:
					if status == _in_progress {
						// Wait more
						continue
					} else {
						// AlreadySent or NotReceived: retry
						retries++
					}
				case <-time.After(10 * time.Second):
					retries++
				}
				if retries >= maxRetries {
					fmt.Printf("Chunk %d failed after %d retries, continuing to next (uuid=%s)\n", idx, retries, uuid)
					break
				}
			}
			replyIs2 := make(chan any)
			c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Key: uuid, Idx: idx, Reply: replyIs2}
			got := (<-replyIs2).(bool)
			if got {
				break
			}
			if retries >= maxRetries {
				break
			}
		}
	}
	replyCount := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getCount", Key: uuid, Reply: replyCount}
	totalRec := (<-replyCount).(int)
	if totalRec >= totalChunks {
		c.packetGenerator(_transfer_complete, []byte(uuid), 0, nil, nil)
	} else {
		fmt.Printf("File %s partially received (%d/%d). You may retry later.\n", uuid, totalRec, totalChunks)
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
		fmt.Println("Metadata ack received by server, waiting for chunk requests")
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting metadata ack")
	}
	return nil
}
func (c *Client) handleRequestChunk(payload []byte, clientAckPacketId uint16) {
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
		fmt.Printf("Received request for unknown file %s\n", uuid)
		c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil, nil)
		return
	}
	c.serveStateChan <- ServeCommand{Action: "setRequested", Key: uuid, Idx: idx}
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
					c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil, nil)
					return
				}
				end := start + int64(res.M.ChunkSize)
				if end > fileSize {
					end = fileSize
				}
				buf = buf[:end-start]
			} else {
				c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil, nil)
				return
			}
		} else {
			c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, uuid)), clientAckPacketId, nil, nil)
			return
		}
	}
	payloadChunk := make([]byte, 4+UUIDLen+len(buf))
	binary.BigEndian.PutUint32(payloadChunk[0:4], uint32(idx))
	copy(payloadChunk[4:4+UUIDLen], []byte(uuid))
	copy(payloadChunk[4+UUIDLen:], buf)
	c.packetGenerator(_chunk, payloadChunk, 0, nil, nil)
	c.serveStateChan <- ServeCommand{Action: "setSent", Key: uuid, Idx: idx}
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
func (c *Client) handleStatusResponse(msgType byte, payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4+UUIDLen {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	uuid := string(payload[4 : 4+UUIDLen])
	c.waitStateChan <- WaitCommand{Action: "notifyStatus", Key: uuid, Idx: idx, Status: msgType}
}
func (c *Client) handlePendingChunk(payload []byte, clientAckPacketId uint16) {
	fmt.Println("Received pending info:", string(payload))
}
func (c *Client) handleTransferComplete(payload []byte, clientAckPacketId uint16) {
	uuid := string(payload)
	replyMeta := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getMeta", Key: uuid, Reply: replyMeta}
	meta := (<-replyMeta).(FileMeta)
	c.serveStateChan <- ServeCommand{Action: "closeAndDeleteServe", Key: uuid}
	c.waitStateChan <- WaitCommand{Action: "clear", Key: uuid}
	c.waitStateChan <- WaitCommand{Action: "clearStatus", Key: uuid}
	fmt.Printf("Peer reported transfer complete for %s\n", meta.Filename)
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
				if p, ok := c.pendingPackets[cmd.PacketID]; ok {
					rtt := time.Since(p.LastSend)
					old := c.rttEstimate.Load().(time.Duration)
					newRTT := (old*7 + rtt) / 8
					c.rttEstimate.Store(newRTT)
				}
				delete(c.pendingPackets, cmd.PacketID)
				if ch, ok := c.ackPendingMap[cmd.PacketID]; ok {
					close(ch)
					delete(c.ackPendingMap, cmd.PacketID)
				}
				c.updatePendingSnapshot()
			case "getAllPending":
				snap := c.snapshot.Load().(map[uint16]PendingPacketsJob)
				cmd.Reply <- snap
			case "registerAck":
				if cmd.AckChan != nil {
					c.ackPendingMap[cmd.PacketID] = cmd.AckChan
				}
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
			case "getMeta":
				if m, ok := c.meta[cmd.Key]; ok {
					cmd.Reply <- m
				} else {
					cmd.Reply <- FileMeta{}
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
	for i := 1; i <= 1; i++ {
		go c.writeWorker(i)
	}
	for i := 1; i <= 1; i++ {
		go c.packetGeneratorWorker()
	}
	go c.readWorker()
	for i := 1; i <= 1; i++ {
		go c.packetParserWorker()
	}
	go c.fieldPacketTrackingWorker()
}
func main() {
	client := NewClient("2", "173.208.144.109:10000") //127.0.0.1   //173.208.144.109
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

// package main

// import (
// 	"bufio"
// 	"fmt"
// 	"net"
// 	"os"
// 	"strings"
// 	"time"
// )

// type UDPClient struct {
// 	serverAddr *net.UDPAddr
// 	conn       *net.UDPConn
// }

// func NewUDPClient(server string) (*UDPClient, error) {
// 	addr, err := net.ResolveUDPAddr("udp", server)
// 	if err != nil {
// 		return nil, fmt.Errorf("error resolving server address: %v", err)
// 	}

// 	conn, err := net.DialUDP("udp", nil, addr)
// 	if err != nil {
// 		return nil, fmt.Errorf("error dialing server: %v", err)
// 	}

// 	return &UDPClient{serverAddr: addr, conn: conn}, nil
// }

// func (c *UDPClient) Start() {
// 	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
// 	fmt.Println("Type messages and press Enter (type 'exit' to quit).")

// 	go func() {
// 		buffer := make([]byte, 1024)
// 		for {
// 			n, _, err := c.conn.ReadFromUDP(buffer)
// 			if err != nil {
// 				fmt.Println("Error reading from server:", err)
// 				continue
// 			}
// 			fmt.Println("\n[Server]:", string(buffer[:n]))
// 			fmt.Print(">> ")
// 		}
// 	}()

// 	go func() {
// 		ticker := time.NewTicker(28 * time.Second)
// 		defer ticker.Stop()
// 		for range ticker.C {
// 			_, err := c.conn.Write([]byte("PING"))
// 			if err != nil {
// 				fmt.Println("Error sending keep-alive msg:", err)
// 			}
// 		}
// 	}()

// 	reader := bufio.NewReader(os.Stdin)
// 	for {
// 		fmt.Print(">> ")
// 		text, _ := reader.ReadString('\n')
// 		text = strings.TrimSpace(text)

// 		if text == "exit" {
// 			fmt.Println("Client exiting...")
// 			break
// 		}

// 		_, err := c.conn.Write([]byte(text))
// 		if err != nil {
// 			fmt.Println("Error writing to server:", err)
// 		}
// 	}
// }

// func main() {
// 	client, err := NewUDPClient("127.0.0.1:10000")
// 	if err != nil {
// 		fmt.Println("Error:", err)
// 		return
// 	}
// 	defer client.conn.Close()

// 	client.Start()
// }
