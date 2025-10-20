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
	"sync"
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

	ChunkSize = 1400 //1200
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

type Mutex struct {
	Action   string
	Addr     *net.UDPAddr
	Id       string
	Packet   []byte
	PacketID uint16
	Reply    chan interface{}
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

	muxPending chan Mutex
	snapshot   atomic.Value

	packetIDCounter uint32
	rttEstimate     atomic.Value

	// file receiving
	mux            sync.Mutex
	fileName       string
	totalChunks    int
	chunkSize      int
	receivedCount  int
	fileHandle     *os.File
	receivedChunks map[string]map[int]bool

	// serving files (when this client acts as sender)
	serveMux   sync.Mutex
	serveFiles map[string]*os.File
	serveMeta  map[string]FileMeta

	// per-file chunk waiters (to notify request-manager that chunk arrived)
	waitMux   sync.Mutex
	waitChans map[string]map[int]chan struct{}
}

type FileMeta struct {
	Filename    string
	TotalChunks int
	ChunkSize   int
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
		id:             id,
		serverAddr:     addr,
		conn:           conn,
		writeQueue:     make(chan Job, 5000),
		parseQueue:     make(chan Job, 5000),
		genQueue:       make(chan GenTask, 5000),
		pendingPackets: make(map[uint16]PendingPacketsJob),
		muxPending:     make(chan Mutex, 5000),
		receivedChunks: make(map[string]map[int]bool),

		serveFiles: make(map[string]*os.File),
		serveMeta:  make(map[string]FileMeta),
		waitChans:  make(map[string]map[int]chan struct{}),
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
	//buffer := make([]byte, 65507)
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
		reply := make(chan interface{})
		c.muxPending <- Mutex{Action: "getAllPending", Reply: reply}
		pendings := (<-reply).(map[uint16]PendingPacketsJob)

		for packetID, pending := range pendings {
			rtt := c.rttEstimate.Load().(time.Duration)
			timeout := rtt * 2
			if timeout < 800*time.Millisecond {
				timeout = 800 * time.Millisecond
			}
			if now.Sub(pending.LastSend) >= timeout {
				c.writeQueue <- pending.Job
				c.muxPending <- Mutex{Action: "updatePending", PacketID: packetID}
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
	for task := range c.genQueue {
		packet := make([]byte, 2+2+1+len(task.Payload))

		pid := atomic.AddUint32(&c.packetIDCounter, 1)
		packetID := uint16(pid & 0xFFFF)

		binary.BigEndian.PutUint16(packet[2:4], 0)
		packet[4] = task.MsgType
		copy(packet[5:], task.Payload)

		if task.MsgType != _ack && task.MsgType != _request_chunk && task.MsgType != _pending_chunk && task.MsgType != _transfer_complete {
			binary.BigEndian.PutUint16(packet[0:2], packetID)
			c.muxPending <- Mutex{Action: "addPending", PacketID: packetID, Packet: packet}
			if task.AckChan != nil {
				go func(pid uint16, ackCh chan struct{}) {
					for {
						snap := c.snapshot.Load().(map[uint16]PendingPacketsJob)
						if _, ok := snap[pid]; !ok {
							close(ackCh)
							return
						}
						time.Sleep(100 * time.Millisecond)
					}
				}(packetID, task.AckChan)
			}
			if task.RespChan != nil {
				task.RespChan <- packetID
			}
		} else {
			// for ack or lightweight messages we can use clientAckPacketId
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
		c.muxPending <- Mutex{Action: "deletePending", PacketID: packetID}

	case _metadata:
		c.handleMetadata(payload, packetID)

	case _chunk:
		c.handleChunk(payload, packetID)

	case _request_chunk:
		// peer requested a chunk from us (we act as sender)
		c.handleRequestChunk(payload, packetID)

	case _pending_chunk:
		// peer told us "pending" for some request (treated as soft response)
		c.handlePendingChunk(payload, packetID)

	case _transfer_complete:
		// peer tells us transfer finished (cleanup served file)
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

// ---------------- handle as receiver ----------------

func (c *Client) handleMetadata(payload []byte, clientAckPacketId uint16) {
	// metadata-> filename|totalChunks|chunkSize
	meta := string(payload)
	parts := strings.Split(meta, "|")
	if len(parts) != 3 {
		fmt.Println("Invalid metadata format")
		return
	}

	filename := parts[0]
	totalChunks, _ := strconv.Atoi(parts[1])
	chunkSize, _ := strconv.Atoi(parts[2])

	c.mux.Lock()
	c.fileName = filename
	c.totalChunks = totalChunks
	c.chunkSize = chunkSize
	c.receivedCount = 0
	if c.receivedChunks[c.fileName] == nil {
		c.receivedChunks[c.fileName] = make(map[int]bool)
	}
	c.mux.Unlock()

	// prepare file handle
	fpath := "fromPeer_" + c.fileName
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	c.mux.Lock()
	c.fileHandle = f
	c.mux.Unlock()

	// make wait channels
	c.waitMux.Lock()
	if c.waitChans[c.fileName] == nil {
		c.waitChans[c.fileName] = make(map[int]chan struct{})
	}
	for i := 0; i < totalChunks; i++ {
		c.waitChans[c.fileName][i] = make(chan struct{})
	}
	c.waitMux.Unlock()

	// ack metadata
	c.packetGenerator(_ack, []byte("metadata received"), clientAckPacketId, nil, nil)
	fmt.Printf("Metadata received: %s (%d chunks, %d bytes each)\n", c.fileName, c.totalChunks, c.chunkSize)

	// start request manager to pull chunks from sender
	go c.requestManagerForFile(c.fileName, totalChunks, chunkSize)
}

func (c *Client) handleChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4 {
		return
	}

	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	data := make([]byte, len(payload)-4)
	copy(data, payload[4:])

	c.mux.Lock()
	if c.receivedChunks[c.fileName][idx] {
		c.mux.Unlock()
		fmt.Printf("Duplicate chunk %d ignored\n", idx)
		c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d already received", idx)), clientAckPacketId, nil, nil)
		return
	}
	c.receivedChunks[c.fileName][idx] = true
	c.receivedCount++
	allDone := (c.receivedCount >= c.totalChunks)
	f := c.fileHandle
	chunkSize := c.chunkSize
	filename := c.fileName
	c.mux.Unlock()

	// write quickly
	offset := int64(idx * chunkSize)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}

	// notify waiting requester for this chunk
	c.waitMux.Lock()
	if chmap, ok := c.waitChans[filename]; ok {
		if ch, ok2 := chmap[idx]; ok2 {
			select {
			case <-ch:
				// already closed
			default:
				close(ch)
			}
		}
	}
	c.waitMux.Unlock()

	// ack the chunk (inform sender)
	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil, nil)
	fmt.Printf("Chunk %d received and written (%d/%d)\n", idx, c.receivedCount, c.totalChunks)

	if allDone {
		// cleanup
		f.Close()
		c.mux.Lock()
		if c.fileHandle != nil {
			c.fileHandle = nil
			delete(c.receivedChunks, c.fileName)
		}
		c.mux.Unlock()
		fmt.Printf("File saved from peer: fromPeer_%s\n", filename)

		// inform sender we've completed
		// payload for transfer_complete = filename
		c.packetGenerator(_transfer_complete, []byte(filename), 0, nil, nil)
	}
}

func (c *Client) requestManagerForFile(filename string, totalChunks int, chunkSize int) {
	// simple sequential manager with retry
	timeout := 60 * time.Second
	maxRetries := 5

	for idx := 0; idx < totalChunks; idx++ {
		retries := 0
		for {
			// if already received, break
			c.mux.Lock()
			already := c.receivedChunks[filename][idx]
			c.mux.Unlock()
			if already {
				break
			}

			// send request packet (payload: 4-byte idx + filename maybe optional)
			payload := make([]byte, 4+len(filename))
			binary.BigEndian.PutUint32(payload[0:4], uint32(idx))
			copy(payload[4:], []byte(filename))
			c.packetGenerator(_request_chunk, payload, 0, nil, nil)

			// wait on waitChans for this chunk
			c.waitMux.Lock()
			ch := c.waitChans[filename][idx]
			c.waitMux.Unlock()

			select {
			case <-ch:
				// received
				break
			case <-time.After(timeout):
				retries++
				if retries >= maxRetries {
					fmt.Printf("Chunk %d failed after %d retries, continuing to next (filename=%s)\n", idx, retries, filename)
					// allow to try again later (or we could signal error). We'll try one more time then continue.
					// For now continue to next chunk, leaving it missing.
					break
				} else {
					// retry (loop)
					// continue
				}
			}
			// if we broke due to receive, exit inner loop, else if timed out and retries exceed -> exit inner loop too
			c.mux.Lock()
			if c.receivedChunks[filename][idx] {
				c.mux.Unlock()
				break
			}
			c.mux.Unlock()
			if retries >= maxRetries {
				// continue to next chunk
				break
			}
		}
	}

	// after scanning all, check if all received
	c.mux.Lock()
	totalRec := 0
	for k := range c.receivedChunks[filename] {
		if c.receivedChunks[filename][k] {
			totalRec++
		}
	}
	c.mux.Unlock()

	if totalRec >= totalChunks {
		// already sent transfer_complete in handleChunk when allDone
	} else {
		// partial: still attempt to request missing ones again
		fmt.Printf("File %s partially received (%d/%d). You may retry later.\n", filename, totalRec, totalChunks)
	}
}

// ---------------- serve (act as sender) ----------------

func (c *Client) SendFileToServer(path string) error {
	// open and register file to be served on request
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

	c.serveMux.Lock()
	c.serveFiles[filename] = f
	c.serveMeta[filename] = FileMeta{Filename: filename, TotalChunks: totalChunks, ChunkSize: ChunkSize}
	c.serveMux.Unlock()

	metadataStr := fmt.Sprintf("%s|%d|%d", filename, totalChunks, ChunkSize)
	metaAck := make(chan struct{})
	c.packetGenerator(_metadata, []byte(metadataStr), 0, metaAck, nil)

	// wait ack or timeout
	select {
	case <-metaAck:
		fmt.Println("Metadata ack received by server, waiting for chunk requests")
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting metadata ack")
	}
	return nil
}

// handle incoming requests for chunks from peers
func (c *Client) handleRequestChunk(payload []byte, clientAckPacketId uint16) {
	// payload: 4-byte idx + filename
	if len(payload) < 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	filename := string(payload[4:])

	c.serveMux.Lock()
	f, okf := c.serveFiles[filename]
	meta, okm := c.serveMeta[filename]
	c.serveMux.Unlock()

	if !okf || !okm {
		// not found, ignore or send ack
		fmt.Printf("Received request for unknown file %s\n", filename)
		// send pending to tell receiver to retry later
		c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
		return
	}

	// try to read chunk data
	offset := int64(idx * meta.ChunkSize)
	buf := make([]byte, meta.ChunkSize)
	_, err := f.ReadAt(buf, offset)
	if err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// maybe it's the last chunk and smaller
			// adjust length: find actual bytes by reading from file size
			stat, stErr := f.Stat()
			if stErr == nil {
				fileSize := stat.Size()
				start := offset
				if start >= fileSize {
					// non-existent chunk
					c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
					return
				}
				end := start + int64(meta.ChunkSize)
				if end > fileSize {
					end = fileSize
				}
				buf = buf[:end-start]
			} else {
				// inform pending
				c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
				return
			}
		} else {
			// cannot read
			c.packetGenerator(_pending_chunk, []byte(fmt.Sprintf("%d|%s", idx, filename)), clientAckPacketId, nil, nil)
			return
		}
	}

	// prepare chunk payload
	payloadChunk := make([]byte, 4+len(buf))
	binary.BigEndian.PutUint32(payloadChunk[0:4], uint32(idx))
	copy(payloadChunk[4:], buf)

	// send chunk
	c.packetGenerator(_chunk, payloadChunk, 0, nil, nil)
}

// handle pending chunk (peer told us to wait)
func (c *Client) handlePendingChunk(payload []byte, clientAckPacketId uint16) {
	// payload contains idx|filename as string; we simply log and let request manager retry
	fmt.Println("Received pending info:", string(payload))
}

// handle transfer complete from peer (cleanup any serve resources maybe)
func (c *Client) handleTransferComplete(payload []byte, clientAckPacketId uint16) {
	filename := string(payload)
	c.serveMux.Lock()
	if f, ok := c.serveFiles[filename]; ok {
		f.Close()
		delete(c.serveFiles, filename)
	}
	delete(c.serveMeta, filename)
	c.serveMux.Unlock()
	fmt.Printf("Peer reported transfer complete for %s\n", filename)
}

// ---------------- mutex pending ----------------

func (c *Client) MutexHandleActions() {
	for mu := range c.muxPending {
		switch mu.Action {
		case "addPending":
			c.pendingPackets[mu.PacketID] = PendingPacketsJob{
				Job:      Job{Addr: mu.Addr, Packet: mu.Packet},
				LastSend: time.Now(),
			}
			c.updatePendingSnapshot()

		case "updatePending":
			if p, ok := c.pendingPackets[mu.PacketID]; ok {
				p.LastSend = time.Now()
				c.pendingPackets[mu.PacketID] = p
				c.updatePendingSnapshot()
			}

		case "deletePending":
			if p, ok := c.pendingPackets[mu.PacketID]; ok {
				rtt := time.Since(p.LastSend)
				old := c.rttEstimate.Load().(time.Duration)
				newRTT := (old*7 + rtt) / 8
				c.rttEstimate.Store(newRTT)
			}
			delete(c.pendingPackets, mu.PacketID)
			c.updatePendingSnapshot()

		case "getAllPending":
			snap := c.snapshot.Load()
			if snap == nil {
				mu.Reply <- make(map[uint16]PendingPacketsJob)
			} else {
				mu.Reply <- snap.(map[uint16]PendingPacketsJob)
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

	go c.MutexHandleActions()
	go c.fieldPacketTrackingWorker()
}

func main() {
	client := NewClient("2", "173.208.144.109:10000") //173.208.144.109 //127.0.0.1
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
