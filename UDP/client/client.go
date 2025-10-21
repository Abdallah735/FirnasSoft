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

	ChunkSize = 60000 //1200
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
	id             string
	serverAddr     *net.UDPAddr
	conn           *net.UDPConn
	writeQueue     chan Job
	parseQueue     chan Job
	genQueue       chan GenTask
	pendingPackets map[uint16]PendingPacketsJob

	stateChan chan StateCommand
	snapshot  atomic.Value

	packetIDCounter uint32
	rttEstimate     atomic.Value

	// file receiving
	fileName       string
	totalChunks    int
	chunkSize      int
	receivedCount  int
	fileHandle     *os.File
	receivedChunks map[string]map[int]bool

	ackPendingMap map[uint16]chan struct{}

	// serving files (when this client acts as sender)
	serveFiles map[string]*os.File
	serveMeta  map[string]FileMeta

	// per-file chunk waiters (to notify request-manager that chunk arrived)
	waitChans map[string]map[int]chan struct{}

	receivingStateChan chan ReceivingCommand
	serveStateChan     chan ServeCommand
	waitStateChan      chan WaitCommand
}

type ReceivingCommand struct {
	Action      string
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
	Action   string
	Filename string
	Meta     FileMeta
	File     *os.File
	Reply    chan any
}

type WaitCommand struct {
	Action   string
	Filename string
	Idx      int
	Reply    chan any
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
		receivedChunks:     make(map[string]map[int]bool),
		ackPendingMap:      make(map[uint16]chan struct{}),
		serveFiles:         make(map[string]*os.File),
		serveMeta:          make(map[string]FileMeta),
		waitChans:          make(map[string]map[int]chan struct{}),
		receivingStateChan: make(chan ReceivingCommand, 100),
		serveStateChan:     make(chan ServeCommand, 100),
		waitStateChan:      make(chan WaitCommand, 100),
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

		if task.MsgType != _ack && task.MsgType != _request_chunk && task.MsgType != _pending_chunk && task.MsgType != _transfer_complete {
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
	if len(parts) != 3 {
		fmt.Println("Invalid metadata format")
		return
	}

	filename := parts[0]
	totalChunks, _ := strconv.Atoi(parts[1])
	chunkSize, _ := strconv.Atoi(parts[2])

	fpath := "fromPeer_" + filename
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}

	replyCh := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "setMetadata", Filename: filename, TotalChunks: totalChunks, ChunkSize: chunkSize, File: f, Reply: replyCh}
	_ = (<-replyCh).(bool)

	c.waitStateChan <- WaitCommand{Action: "ensureFileChans", Filename: filename} // new action to make all chans

	c.packetGenerator(_ack, []byte("metadata received"), clientAckPacketId, nil, nil)
	fmt.Printf("Metadata received: %s (%d chunks, %d bytes each)\n", filename, totalChunks, chunkSize)

	go c.requestManagerForFile(filename, totalChunks, chunkSize)
}

func (c *Client) handleChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4 {
		return
	}

	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	data := make([]byte, len(payload)-4)
	copy(data, payload[4:])

	replyIs := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Idx: idx, Reply: replyIs}
	isDup := (<-replyIs).(bool)
	if isDup {
		fmt.Printf("Duplicate chunk %d ignored\n", idx)
		c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d already received", idx)), clientAckPacketId, nil, nil)
		return
	}

	c.receivingStateChan <- ReceivingCommand{Action: "setReceived", Idx: idx}

	replyFile := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getFile", Reply: replyFile}
	f := (<-replyFile).(*os.File)

	replySize := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getChunkSize", Reply: replySize}
	size := (<-replySize).(int)

	replyName := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getFilename", Reply: replyName}
	filename := (<-replyName).(string)

	offset := int64(idx * size)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}

	c.waitStateChan <- WaitCommand{Action: "notify", Filename: filename, Idx: idx}

	c.receivingStateChan <- ReceivingCommand{Action: "incrementCount"}

	replyCount := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getCount", Reply: replyCount}
	count := (<-replyCount).(int)

	replyTotal := make(chan any)
	c.receivingStateChan <- ReceivingCommand{Action: "getTotal", Reply: replyTotal}
	total := (<-replyTotal).(int)

	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil, nil)
	fmt.Printf("Chunk %d received and written (%d/%d)\n", idx, count, total)

	if count >= total {
		c.receivingStateChan <- ReceivingCommand{Action: "closeFile"}
		fmt.Printf("File saved from peer: fromPeer_%s\n", filename)

		c.packetGenerator(_transfer_complete, []byte(filename), 0, nil, nil)
		c.waitStateChan <- WaitCommand{Action: "clear", Filename: filename}
	}
}

func (c *Client) requestManagerForFile(filename string, totalChunks int, chunkSize int) {
	timeout := 60 * time.Second
	maxRetries := 5

	for idx := 0; idx < totalChunks; idx++ {
		retries := 0
		for {
			replyIs := make(chan any)
			c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Idx: idx, Reply: replyIs}
			already := (<-replyIs).(bool)
			if already {
				break
			}

			payload := make([]byte, 4+len(filename))
			binary.BigEndian.PutUint32(payload[0:4], uint32(idx))
			copy(payload[4:], []byte(filename))
			c.packetGenerator(_request_chunk, payload, 0, nil, nil)

			replyW := make(chan any)
			c.waitStateChan <- WaitCommand{Action: "ensureChan", Filename: filename, Idx: idx, Reply: replyW}
			ch := (<-replyW).(chan struct{})

			select {
			case <-ch:
				break
			case <-time.After(timeout):
				retries++
				if retries >= maxRetries {
					fmt.Printf("Chunk %d failed after %d retries, continuing to next (filename=%s)\n", idx, retries, filename)
					break
				}
			}

			replyIs2 := make(chan any)
			c.receivingStateChan <- ReceivingCommand{Action: "isReceived", Idx: idx, Reply: replyIs2}
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
	c.receivingStateChan <- ReceivingCommand{Action: "getCount", Reply: replyCount}
	totalRec := (<-replyCount).(int)

	if totalRec >= totalChunks {
		c.packetGenerator(_transfer_complete, []byte(filename), 0, nil, nil)
	} else {
		fmt.Printf("File %s partially received (%d/%d). You may retry later.\n", filename, totalRec, totalChunks)
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

	c.serveStateChan <- ServeCommand{Action: "addServe", Filename: filename, File: f, Meta: FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: ChunkSize}}

	metadataStr := fmt.Sprintf("%s|%d|%d", filename, totalChunks, ChunkSize)
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
	if len(payload) < 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	filename := string(payload[4:])

	replyCh := make(chan any)
	c.serveStateChan <- ServeCommand{Action: "getServe", Filename: filename, Reply: replyCh}
	res := (<-replyCh).(struct {
		F  *os.File
		M  FileMeta
		Ok bool
	})
	if !res.Ok {
		fmt.Printf("Received request for unknown file %s\n", filename)
		c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
		return
	}

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
					c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
					return
				}
				end := start + int64(res.M.ChunkSize)
				if end > fileSize {
					end = fileSize
				}
				buf = buf[:end-start]
			} else {
				c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
				return
			}
		} else {
			c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
			return
		}
	}

	payloadChunk := make([]byte, 4+len(buf))
	binary.BigEndian.PutUint32(payloadChunk[0:4], uint32(idx))
	copy(payloadChunk[4:], buf)

	c.packetGenerator(_chunk, payloadChunk, 0, nil, nil)
}

func (c *Client) handlePendingChunk(payload []byte, clientAckPacketId uint16) {
	fmt.Println("Received pending info:", string(payload))
}

func (c *Client) handleTransferComplete(payload []byte, clientAckPacketId uint16) {
	filename := string(payload)
	c.serveStateChan <- ServeCommand{Action: "closeAndDeleteServe", Filename: filename}
	c.waitStateChan <- WaitCommand{Action: "clear", Filename: filename}
	fmt.Printf("Peer reported transfer complete for %s\n", filename)
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
			case "setMetadata":
				c.fileName = cmd.Filename
				c.totalChunks = cmd.TotalChunks
				c.chunkSize = cmd.ChunkSize
				c.receivedCount = 0
				c.fileHandle = cmd.File
				if c.receivedChunks[cmd.Filename] == nil {
					c.receivedChunks[cmd.Filename] = make(map[int]bool)
				}
				cmd.Reply <- true
			case "isReceived":
				cmd.Reply <- c.receivedChunks[c.fileName][cmd.Idx]
			case "setReceived":
				c.receivedChunks[c.fileName][cmd.Idx] = true
			case "getFile":
				cmd.Reply <- c.fileHandle
			case "getChunkSize":
				cmd.Reply <- c.chunkSize
			case "getFilename":
				cmd.Reply <- c.fileName
			case "incrementCount":
				c.receivedCount++
			case "getCount":
				cmd.Reply <- c.receivedCount
			case "getTotal":
				cmd.Reply <- c.totalChunks
			case "closeFile":
				if c.fileHandle != nil {
					c.fileHandle.Close()
					c.fileHandle = nil
					delete(c.receivedChunks, c.fileName)
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
				c.serveFiles[cmd.Filename] = cmd.File
				c.serveMeta[cmd.Filename] = cmd.Meta
			case "getServe":
				f, okf := c.serveFiles[cmd.Filename]
				m, okm := c.serveMeta[cmd.Filename]
				ok := okf && okm
				cmd.Reply <- struct {
					F  *os.File
					M  FileMeta
					Ok bool
				}{F: f, M: m, Ok: ok}
			case "closeAndDeleteServe":
				if f, ok := c.serveFiles[cmd.Filename]; ok {
					f.Close()
				}
				delete(c.serveFiles, cmd.Filename)
				delete(c.serveMeta, cmd.Filename)
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
				if _, ok := c.waitChans[cmd.Filename]; !ok {
					c.waitChans[cmd.Filename] = make(map[int]chan struct{})
				}
				if _, ok := c.waitChans[cmd.Filename][cmd.Idx]; !ok {
					c.waitChans[cmd.Filename][cmd.Idx] = make(chan struct{})
				}
				cmd.Reply <- c.waitChans[cmd.Filename][cmd.Idx]
			case "notify":
				if chmap, ok := c.waitChans[cmd.Filename]; ok {
					if ch, ok2 := chmap[cmd.Idx]; ok2 {
						select {
						case <-ch:
						default:
							close(ch)
						}
						delete(c.waitChans[cmd.Filename], cmd.Idx)
					}
				}
			case "ensureFileChans":
				if _, ok := c.waitChans[cmd.Filename]; !ok {
					c.waitChans[cmd.Filename] = make(map[int]chan struct{})
				}
				// assuming we don't create all at once, but code had for i=0 to total make chan
				// to simplify, perhaps create when needed in ensureChan
			case "clear":
				if chmap, ok := c.waitChans[cmd.Filename]; ok {
					for _, ch := range chmap {
						close(ch)
					}
					delete(c.waitChans, cmd.Filename)
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

func (c *Client) Start() {
	go c.StateHandler()
	go c.ReceivingStateHandler()
	go c.ServeStateHandler()
	go c.WaitStateHandler()

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
	client := NewClient("2", "127.0.0.1:10000") //127.0.0.1
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
