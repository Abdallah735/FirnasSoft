// === client.go ===
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
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
	_register = 1
	_ping     = 2
	_message  = 3
	_ack      = 4
	_metadata = 5
	_chunk    = 6

	ChunkSize = 65507 - (2 + 2 + 1 + 4)
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
	id               string
	serverAddr       *net.UDPAddr
	conn             *net.UDPConn
	writeQueue       chan Job
	parseQueue       chan Job
	genQueue         chan GenTask
	pendingPackets   map[uint16]PendingPacketsJob
	muxPending       chan Mutex
	snapshot         atomic.Value
	pendingSendTimes map[uint16]time.Time
	chunkSize        int
	muChunkSize      sync.Mutex
	lastRTT          time.Duration
	lastBandwidth    float64
	lossRate         float64
	// file receiving
	mux                sync.Mutex
	pendingSendTimesMu sync.Mutex
	fileName           string
	totalChunks        int
	receivedCount      int
	fileHandle         *os.File
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
		id:               id,
		serverAddr:       addr,
		conn:             conn,
		writeQueue:       make(chan Job, 5000),
		parseQueue:       make(chan Job, 5000),
		genQueue:         make(chan GenTask, 5000),
		pendingPackets:   make(map[uint16]PendingPacketsJob),
		muxPending:       make(chan Mutex, 5000),
		snapshot:         atomic.Value{},
		pendingSendTimes: make(map[uint16]time.Time),
		chunkSize:        1200,
		lastRTT:          100 * time.Millisecond,
		lastBandwidth:    1000000,
		lossRate:         0.0,
	}
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
	buffer := make([]byte, 65507)
	for {
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
			if now.Sub(pending.LastSend) >= 1*time.Second {
				c.writeQueue <- pending.Job
				c.muxPending <- Mutex{Action: "updatePending", PacketID: packetID}
			}
			time.Sleep(30 * time.Millisecond)
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
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for task := range c.genQueue {
		packet := make([]byte, 2+2+1+len(task.Payload))

		packetID := uint16(r.Intn(65535))

		binary.BigEndian.PutUint16(packet[2:4], 0)
		packet[4] = task.MsgType
		copy(packet[5:], task.Payload)

		if task.MsgType != _ack {
			binary.BigEndian.PutUint16(packet[0:2], packetID)
			c.muxPending <- Mutex{Action: "addPending", PacketID: packetID, Packet: packet}
			c.pendingSendTimesMu.Lock()
			c.pendingSendTimes[packetID] = time.Now()
			c.pendingSendTimesMu.Unlock()
			if task.AckChan != nil {
				go func(pid uint16, ackCh chan struct{}) {
					for {
						snap := c.snapshot.Load()
						if snap == nil {
							// no pending
							close(ackCh)
							return
						}
						pendingMap := snap.(map[uint16]PendingPacketsJob)
						if _, ok := pendingMap[pid]; !ok {
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
		fmt.Println("Server ack:", string(payload))
		c.pendingSendTimesMu.Lock()
		sendTime, ok := c.pendingSendTimes[packetID]
		if ok {
			rtt := time.Since(sendTime)
			c.muChunkSize.Lock()
			c.lastRTT = rtt
			c.muChunkSize.Unlock()
			delete(c.pendingSendTimes, packetID)
		}
		c.pendingSendTimesMu.Unlock()

		c.muxPending <- Mutex{Action: "deletePending", PacketID: packetID}

	case _metadata:
		c.handleMetadata(payload, packetID)

	case _chunk:
		c.handleChunk(payload, packetID)
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

	c.mux.Lock()
	c.fileName = parts[0]
	c.totalChunks, _ = strconv.Atoi(parts[1])
	c.chunkSize, _ = strconv.Atoi(parts[2])
	c.receivedCount = 0
	c.mux.Unlock()

	c.packetGenerator(_ack, []byte("metadata received"), clientAckPacketId, nil, nil)
	fmt.Printf("Metadata received: %s (%d chunks, %d bytes each)\n", c.fileName, c.totalChunks, c.chunkSize)

	f, err := os.OpenFile(c.fileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file for writing:", err)
		return
	}
	c.mux.Lock()
	c.fileHandle = f
	c.mux.Unlock()
}

func (c *Client) handleChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4 {
		return
	}

	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	data := make([]byte, len(payload)-4)
	copy(data, payload[4:])

	c.mux.Lock()
	f := c.fileHandle
	filename := c.fileName
	c.receivedCount++
	allDone := (c.receivedCount >= c.totalChunks)
	c.mux.Unlock()

	if f == nil {
		fmt.Println("File handle is nil!")
		return
	}

	offset := int64(idx * c.chunkSize)
	_, err := f.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Error writing chunk:", err)
		return
	}

	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d received", idx)), clientAckPacketId, nil, nil)
	fmt.Printf("Chunk %d received and written (%d/%d)\n", idx, c.receivedCount, c.totalChunks)

	if allDone {
		c.mux.Lock()
		if c.fileHandle != nil {
			c.fileHandle.Close()
			c.fileHandle = nil
		}
		c.mux.Unlock()
		fmt.Println("File received:", filename)
	}
}

func (c *Client) measureRTT() time.Duration {
	rtts := make([]time.Duration, 0, 5)
	count := 5
	for i := 0; i < count; i++ {
		respChan := make(chan uint16, 1)
		c.packetGenerator(_ping, []byte("ping"), 0, nil, respChan)
		packetID := <-respChan
		start := time.Now()
		time.Sleep(200 * time.Millisecond)
		for j := 0; j < 10; j++ {
			if _, exists := c.pendingSendTimes[packetID]; !exists {
				rtts = append(rtts, time.Since(start))
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if len(rtts) == 0 {
		return c.lastRTT
	}
	var sum time.Duration
	for _, rtt := range rtts {
		sum += rtt
	}
	return sum / time.Duration(len(rtts))
}

func (c *Client) measureBandwidth() float64 {
	testSize := 1000
	count := 10
	start := time.Now()
	for i := 0; i < count; i++ {
		payload := make([]byte, testSize)
		c.packetGenerator(_message, payload, 0, nil, nil)
		time.Sleep(50 * time.Millisecond)
	}
	elapsed := time.Since(start).Seconds()
	totalBytes := float64(testSize * count)
	return (totalBytes * 8) / elapsed // bits/sec
}

func (c *Client) measurePathMTU() int {
	return 1500 // Fallback to standard MTU
}

func (c *Client) measureLossRate() float64 {
	reply := make(chan interface{})
	c.muxPending <- Mutex{Action: "getAllPending", Reply: reply}
	pendings := (<-reply).(map[uint16]PendingPacketsJob)
	totalPending := len(pendings)
	if totalPending == 0 {
		return 0.0
	}
	return c.lossRate // Placeholder; improve by tracking retransmits
}

func (c *Client) networkMonitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rtt := c.measureRTT()
		bandwidth := c.measureBandwidth()
		mtu := c.measurePathMTU()
		loss := c.measureLossRate()

		bdp := (bandwidth * rtt.Seconds()) / 8 // bytes
		maxChunk := mtu - 28                   // IP+UDP headers

		newChunkSize := int(bdp / 10)
		if loss > 0.05 {
			newChunkSize = int(float64(newChunkSize) * 0.8)
		}
		newChunkSize = min(newChunkSize, maxChunk)

		c.muChunkSize.Lock()
		c.chunkSize = max(newChunkSize, 512)
		c.lastRTT = rtt
		c.lastBandwidth = bandwidth
		c.lossRate = loss
		c.muChunkSize.Unlock()

		c.conn.SetReadBuffer(int(bdp * 1.5))
		c.conn.SetWriteBuffer(int(bdp * 1.5))

		fmt.Printf("Updated: chunkSize=%d, RTT=%v, bandwidth=%.2f Mbps, loss=%.2f%%\n",
			c.chunkSize, rtt, bandwidth/1e6, loss*100)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *Client) SendFileToServer(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	fileSize := stat.Size()
	c.muChunkSize.Lock()
	chunkSz := c.chunkSize
	c.muChunkSize.Unlock()

	totalChunks := int((fileSize + int64(chunkSz) - 1) / int64(chunkSz))
	filename := filepath.Base(path)
	metadataStr := fmt.Sprintf("%s|%d|%d", filename, totalChunks, chunkSz)

	metaAck := make(chan struct{})
	c.packetGenerator(_metadata, []byte(metadataStr), 0, metaAck, nil)

	select {
	case <-metaAck:
		fmt.Println("Metadata ack received, starting file transfer")
	case <-time.After(20 * time.Second):
		return fmt.Errorf("timeout waiting metadata ack")
	}

	buf := make([]byte, chunkSz)
	for chunkIndex := 0; chunkIndex < totalChunks; chunkIndex++ {
		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		chunkData := make([]byte, n)
		copy(chunkData, buf[:n])

		payload := make([]byte, 4+len(chunkData))
		binary.BigEndian.PutUint32(payload[0:4], uint32(chunkIndex))
		copy(payload[4:], chunkData)

		c.packetGenerator(_chunk, payload, 0, nil, nil)
		time.Sleep(30 * time.Millisecond)
	}
	return nil
}

func (c *Client) Start() {
	for i := 1; i <= 4; i++ {
		go c.writeWorker(i)
	}
	for i := 1; i <= 4; i++ {
		go c.packetGeneratorWorker()
	}
	go c.readWorker()
	for i := 1; i <= 4; i++ {
		go c.packetParserWorker()
	}
	go c.MutexHandleActions()
	go c.fieldPacketTrackingWorker()
	go c.networkMonitor()
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
