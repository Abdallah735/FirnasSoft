// === client.go ===
// client.go (modified)
// يعتمد على كودك الأصلي مع تحسينات: fixed chunk cap, token-bucket pacing,
// EWMA smoothing for RTT/BW/Loss, faster networkMonitor, retransmit counting.

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

	// safety chunk cap to avoid MTU fragmentation (leave headroom for IP/UDP + our headers)
	ChunkCap = 1200
	// internal max UDP payload used in buffers
	MaxUDPPayload = 65507
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

	// tunables / stats
	chunkSize     int
	muChunkSize   sync.Mutex
	lastRTT       time.Duration
	lastBandwidth float64 // bits per second
	lossRate      float64

	// retransmit accounting
	retransmitCounts   map[uint16]int
	retransmitCountsMu sync.Mutex

	// file receiving
	mux           sync.Mutex
	fileName      string
	totalChunks   int
	receivedCount int
	fileHandle    *os.File
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
		chunkSize:        ChunkCap, // start at cap
		lastRTT:          100 * time.Millisecond,
		lastBandwidth:    1e6, // 1 Mbps default
		lossRate:         0.0,
		retransmitCounts: make(map[uint16]int),
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
	buffer := make([]byte, MaxUDPPayload)
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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		reply := make(chan interface{})
		c.muxPending <- Mutex{Action: "getAllPending", Reply: reply}
		pendings := (<-reply).(map[uint16]PendingPacketsJob)

		for packetID, pending := range pendings {
			// retransmit logic: if > 1s since last send, retransmit
			if now.Sub(pending.LastSend) >= 1*time.Second {
				// increment retransmit counter
				c.retransmitCountsMu.Lock()
				c.retransmitCounts[packetID]++
				c.retransmitCountsMu.Unlock()

				// resend
				c.writeQueue <- pending.Job
				c.muxPending <- Mutex{Action: "updatePending", PacketID: packetID}
			}
			// small sleep to avoid tight loop
			time.Sleep(5 * time.Millisecond)
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
			// add to pending queue
			c.muxPending <- Mutex{Action: "addPending", PacketID: packetID, Packet: packet, Addr: c.serverAddr}
			c.pendingSendTimes[packetID] = time.Now()
			if task.AckChan != nil {
				// goroutine signals ackChan when pending removed
				go func(pid uint16, ackCh chan struct{}) {
					for {
						snap := c.snapshot.Load()
						if snap == nil {
							close(ackCh)
							return
						}
						pendingMap := snap.(map[uint16]PendingPacketsJob)
						if _, ok := pendingMap[pid]; !ok {
							close(ackCh)
							return
						}
						time.Sleep(50 * time.Millisecond)
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
		sendTime, ok := c.pendingSendTimes[packetID]
		if ok {
			rtt := time.Since(sendTime)
			// EWMA smoothing for RTT
			c.muChunkSize.Lock()
			c.lastRTT = time.Duration(0.8*float64(c.lastRTT) + 0.2*float64(rtt))
			c.muChunkSize.Unlock()
			delete(c.pendingSendTimes, packetID)
		}
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
	if c.chunkSize > ChunkCap {
		c.chunkSize = ChunkCap
	}
	p := "recv_" + c.fileName
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Println("Error opening file:", err)
		c.mux.Unlock()
		return
	}
	c.fileHandle = f
	c.receivedCount = 0
	c.mux.Unlock()

	// ack
	c.packetGenerator(_ack, []byte("metadata ok"), clientAckPacketId, nil, nil)
	fmt.Printf("Receiving file: %s, chunks=%d, chunkSize=%d\n", c.fileName, c.totalChunks, c.chunkSize)
}

func (c *Client) handleChunk(payload []byte, clientAckPacketId uint16) {
	if len(payload) < 4 {
		return
	}
	idx := int(binary.BigEndian.Uint32(payload[0:4]))
	data := make([]byte, len(payload)-4)
	copy(data, payload[4:])

	c.mux.Lock()
	defer c.mux.Unlock()
	if c.fileHandle == nil {
		// still ack to help sender
		c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d recv (no file)", idx)), clientAckPacketId, nil, nil)
		return
	}
	offset := int64(idx * c.chunkSize)
	_, err := c.fileHandle.WriteAt(data, offset)
	if err != nil {
		fmt.Println("Write error:", err)
		return
	}
	c.receivedCount++
	c.packetGenerator(_ack, []byte(fmt.Sprintf("chunk %d recv", idx)), clientAckPacketId, nil, nil)
	if c.receivedCount >= c.totalChunks {
		c.fileHandle.Close()
		fmt.Printf("File saved: recv_%s\n", c.fileName)
		c.fileHandle = nil
	}
}

// ------------------ Measurement helpers ------------------

func (c *Client) measureRTT() time.Duration {
	// send a small ping packet and wait for its ack
	metaAck := make(chan struct{})
	resp := make(chan uint16)
	c.packetGenerator(_ping, []byte("rtt_probe"), 0, metaAck, resp)

	// wait for ack to be closed by packetGenerator's goroutine or timeout
	select {
	case <-metaAck:
		// RTT already updated in PacketParser via send times
	case <-time.After(3 * time.Second):
		// timeout
	}

	c.muChunkSize.Lock()
	rtt := c.lastRTT
	c.muChunkSize.Unlock()
	return rtt
}

func (c *Client) measureBandwidthAndLoss(sampleBytes int, numPackets int) (float64, float64) {
	// send numPackets of payload size sampleBytes and measure time until their ACKs arrive.
	// returns bandwidth (bits/sec) and lossRate (fraction)
	if numPackets <= 0 {
		numPackets = 20
	}
	if sampleBytes <= 0 {
		c.muChunkSize.Lock()
		sampleBytes = c.chunkSize
		c.muChunkSize.Unlock()
	}

	// prepare to track the packetIDs we create
	packetIDs := make([]uint16, 0, numPackets)
	//respChans := make([]chan uint16, 0, numPackets)
	ackChans := make([]chan struct{}, 0, numPackets)

	for i := 0; i < numPackets; i++ {
		// payload marker to indicate probe (server will ack as usual)
		payload := make([]byte, sampleBytes)
		// fill with arbitrary data
		for j := range payload {
			payload[j] = byte((i + j) & 0xFF)
		}
		ack := make(chan struct{})
		resp := make(chan uint16, 1)
		c.packetGenerator(_message, payload, 0, ack, resp) // server will send ACK
		// receive packetID reference quickly
		var pid uint16
		select {
		case pid = <-resp:
		case <-time.After(500 * time.Millisecond):
			// if we didn't get a pid (shouldn't happen), continue
			continue
		}
		packetIDs = append(packetIDs, pid)
		//respChans = append(respChans, resp)
		ackChans = append(ackChans, ack)
	}

	// wait until all ack chans close or timeout
	start := time.Now()
	timeout := time.After(5 * time.Second)
	done := make(chan struct{})
	go func() {
		for _, ch := range ackChans {
			<-ch // will be closed by packetGenerator ack-watcher when ack received
		}
		close(done)
	}()

	select {
	case <-done:
		// all acked
	case <-timeout:
		// partial
	}

	elapsed := time.Since(start)
	// compute number of retransmits for those packetIDs
	c.retransmitCountsMu.Lock()
	var retransmits int
	for _, pid := range packetIDs {
		if cnt, ok := c.retransmitCounts[pid]; ok {
			retransmits += cnt
			// cleanup
			delete(c.retransmitCounts, pid)
		}
	}
	c.retransmitCountsMu.Unlock()

	//acked := len(packetIDs) - retransmits // approximation: if retransmits >0 we treat as lost at least once
	if elapsed <= 0 {
		elapsed = time.Millisecond
	}
	totalBytes := float64(len(packetIDs) * sampleBytes)
	bandwidthBps := (totalBytes * 8.0) / elapsed.Seconds()

	// smoothing with EWMA
	c.muChunkSize.Lock()
	c.lastBandwidth = 0.7*c.lastBandwidth + 0.3*bandwidthBps
	c.muChunkSize.Unlock()

	// estimate loss = retransmits / sent (this is an approximation)
	loss := 0.0
	if len(packetIDs) > 0 {
		loss = float64(retransmits) / float64(len(packetIDs))
		c.muChunkSize.Lock()
		c.lossRate = 0.8*c.lossRate + 0.2*loss
		c.muChunkSize.Unlock()
	}

	return c.lastBandwidth, c.lossRate
}

func (c *Client) networkMonitor() {
	// quicker monitoring to adapt fast
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		// measure RTT quickly
		rtt := c.measureRTT()

		// measure bandwidth with modest probe: 16KB packets x 8 = ~128KB total
		c.muChunkSize.Lock()
		probeSize := c.chunkSize
		c.muChunkSize.Unlock()
		// ensure probe not huge
		if probeSize > 4096 {
			probeSize = 4096
		}
		bw, loss := c.measureBandwidthAndLoss(probeSize, 8)

		// compute BDP and choose chunk size (but cap by ChunkCap and leave header margin)
		bdp := (bw * rtt.Seconds()) / 8.0 // bytes
		maxChunk := ChunkCap - 28         // safe margin for headers

		// conservative rule: chunk = max(512, min( maxChunk, bdp/10))
		newChunk := int(bdp / 10.0)
		if newChunk < 512 {
			newChunk = 512
		}
		if newChunk > maxChunk {
			newChunk = maxChunk
		}
		// reduce chunk on loss
		if loss > 0.05 {
			newChunk = int(float64(newChunk) * 0.8)
		}

		// EWMA smoothing on stored stats is already inside measure funcs
		c.muChunkSize.Lock()
		c.chunkSize = newChunk
		c.lastRTT = rtt
		c.lastBandwidth = bw
		c.lossRate = loss
		c.muChunkSize.Unlock()

		// adjust socket buffers (best-effort)
		bdpInt := int(bdp)
		if bdpInt < 4096 {
			bdpInt = 4096
		}
		_ = c.conn.SetReadBuffer(bdpInt * 2)
		_ = c.conn.SetWriteBuffer(bdpInt * 2)

		fmt.Printf("[NETMON] chunk=%d RTT=%v BW=%.2fMbps loss=%.2f%%\n",
			c.chunkSize, c.lastRTT, c.lastBandwidth/1e6, c.lossRate*100)
	}
}

// ---------------- File send with token-bucket pacing ----------------

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

	// get starting chunk size (cap)
	c.muChunkSize.Lock()
	chunkSz := c.chunkSize
	c.muChunkSize.Unlock()
	if chunkSz <= 0 || chunkSz > ChunkCap {
		chunkSz = ChunkCap
	}

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

	// token-bucket
	c.muChunkSize.Lock()
	sendRateBps := c.lastBandwidth // bits/sec
	c.muChunkSize.Unlock()
	sendRate := int64(sendRateBps / 8.0) // bytes/sec
	if sendRate <= 0 {
		sendRate = 100 * 1024 // default 100KB/s
	}
	tokens := int64(0)
	bucketCap := sendRate * 2
	refillInterval := 50 * time.Millisecond
	ticker := time.NewTicker(refillInterval)
	defer ticker.Stop()

	// goroutine to keep updating sendRate from lastBandwidth
	stopUpdate := make(chan struct{})
	go func() {
		t := time.NewTicker(700 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.muChunkSize.Lock()
				updated := int64(c.lastBandwidth / 8.0)
				c.muChunkSize.Unlock()
				if updated > 0 {
					sendRate = updated
					// ensure bucket cap reasonable
					if sendRate*2 < int64(chunkSz*4) {
						bucketCap = int64(chunkSz * 4)
					} else {
						bucketCap = sendRate * 2
					}
				}
			case <-stopUpdate:
				return
			}
		}
	}()

	buf := make([]byte, chunkSz)
	chunkIndex := 0

	for chunkIndex < totalChunks {
		select {
		case <-ticker.C:
			add := (sendRate * int64(refillInterval)) / int64(time.Second)
			tokens += add
			if tokens > bucketCap {
				tokens = bucketCap
			}
		default:
		}

		// need tokens for at least one chunk
		if tokens < int64(chunkSz) {
			time.Sleep(8 * time.Millisecond)
			continue
		}

		n, err := io.ReadFull(f, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			close(stopUpdate)
			return err
		}
		chunkData := make([]byte, n)
		copy(chunkData, buf[:n])

		payload := make([]byte, 4+len(chunkData))
		binary.BigEndian.PutUint32(payload[0:4], uint32(chunkIndex))
		copy(payload[4:], chunkData)

		// send chunk (pending/ack/retransmit handled by framework)
		c.packetGenerator(_chunk, payload, 0, nil, nil)

		tokens -= int64(n)
		chunkIndex++
	}

	close(stopUpdate)
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
			if ch, ok := c.pendingPackets[mu.PacketID]; ok {
				_ = ch // no-op
			}
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

func main() {
	client := NewClient("2", "127.0.0.1:10000")
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
