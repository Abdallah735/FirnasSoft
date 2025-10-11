// client.go
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
	chunkSize       = 11 * 1024
	numWorkers      = 4
	resendInterval  = 2 * time.Second
	maxAttempts     = 10
	timeoutDuration = 60 * time.Second
	sendPace        = 40 * time.Millisecond
)

// ----------------- CommunicationManager (client-side) -----------------
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
	_, err := cm.conn.Write(data) // conn is dialed to server — Write is fine
	return err
}

// ----------------- IncomingPacket -----------------
type IncomingPacket struct {
	addr *net.UDPAddr
	data []byte
}

// ----------------- Pending / PacketTracker -----------------
type Pending struct {
	data     []byte
	addr     *net.UDPAddr
	sendTime time.Time
	attempts int
}

type addReq struct {
	fileID     string
	chunkIndex int
	data       []byte
	addr       *net.UDPAddr
	ackCh      chan struct{}
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
	pending map[string]*Pending

	addCh    chan addReq
	removeCh chan removeReq
	hasCh    chan hasReq
	hasAnyCh chan hasAnyReq

	sendQueue chan SendTask

	resendInterval time.Duration
	maxAttempts    int
	timeout        time.Duration
	comm           *CommunicationManager
	waiters        map[string][]chan struct{}
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
	for i := 0; i < pt.workerCount; i++ {
		go func(id int) {
			for task := range pt.sendQueue {
				if err := pt.comm.Send(task.addr, task.data); err != nil {
					fmt.Printf("[send worker %d] Error sending packet %s_%d: %v\n", id, task.fileID, task.chunkIndex, err)
				} else {
					fmt.Printf("[send worker %d] Sent %s_%d -> %s\n", id, task.fileID, task.chunkIndex, task.addr.String())
				}
				time.Sleep(sendPace)
			}
		}(i)
	}

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case req := <-pt.addCh:
				key := fmt.Sprintf("%s_%d", req.fileID, req.chunkIndex)
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
					// NOTE: we removed the timeout-based removal here (so pending removed only by attempts)
					if p.attempts >= pt.maxAttempts {
						fmt.Printf("Failed to deliver packet %s after %d attempts\n", key, p.attempts)
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
					// resend based on resendInterval (keeps a small delay to avoid tight busy-loop)
					if now.Sub(p.sendTime) >= pt.resendInterval {
						p.attempts++
						p.sendTime = now
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

func (pt *PacketTracker) Add(fileID string, chunkIndex int, data []byte, addr *net.UDPAddr) {
	pt.addCh <- addReq{fileID: fileID, chunkIndex: chunkIndex, data: data, addr: addr, ackCh: nil}
}

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

func (pt *PacketTracker) NotifyAck(fileID string, chunkIndex int) {
	pt.removeCh <- removeReq{fileID: fileID, chunkIndex: chunkIndex}
}

func (pt *PacketTracker) HasAnyPending(fileID string) bool {
	resp := make(chan bool)
	pt.hasAnyCh <- hasAnyReq{fileID: fileID, resp: resp}
	return <-resp
}

// ----------------- PacketManager (client-side) -----------------
type PacketManager struct {
	tracker *PacketTracker
	comm    *CommunicationManager
}

func NewPacketManager(comm *CommunicationManager, workerCount int) *PacketManager {
	pt := NewPacketTracker(comm, workerCount)
	pt.Start()
	return &PacketManager{tracker: pt, comm: comm}
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

// ----------------- FileReceiver (for incoming files) -----------------
type FileReceiver struct {
	fileName    string
	totalChunks int
	fileSize    int64
	chunks      map[int][]byte
}

// ----------------- UDPClient -----------------
type UDPClient struct {
	serverAddr *net.UDPAddr
	conn       *net.UDPConn
	comm       *CommunicationManager
	packet     *PacketManager
	receivers  map[string]*FileReceiver

	mu sync.Mutex
	// progress tracking (if sending files)
	fileTotal    map[string]int
	fileEnqueued map[string]int
	fileAcked    map[string]int
}

func NewUDPClient(server string) (*UDPClient, error) {
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, fmt.Errorf("error resolving server address: %v", err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("error dialing server: %v", err)
	}
	comm := NewCommunicationManager(conn)
	pm := NewPacketManager(comm, numWorkers)

	return &UDPClient{
		serverAddr:   addr,
		conn:         conn,
		comm:         comm,
		packet:       pm,
		receivers:    make(map[string]*FileReceiver),
		fileTotal:    make(map[string]int),
		fileEnqueued: make(map[string]int),
		fileAcked:    make(map[string]int),
	}, nil
}

func (c *UDPClient) sendAck(fileID string, chunkIndex int) {
	ackPayload := map[string]interface{}{"file_id": fileID, "chunk_index": chunkIndex}
	ackPacket := c.packet.Generate(Ack, ackPayload)
	_, err := c.conn.Write(ackPacket)
	if err != nil {
		fmt.Println("Error sending ack:", err)
	} else {
		fmt.Println("[CLIENT] Sent ACK", fileID, chunkIndex)
	}
}

// Handle incoming packets and feed tracker ACK notifications
func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	c.comm.StartListen()

	// incoming processing
	go func() {
		for inc := range c.comm.incoming {
			parsed, err := c.packet.Parse(inc.data)
			if err != nil {
				fmt.Printf("Parse error (len %d): %v\n", len(inc.data), err)
				continue
			}
			ptStr, ok := parsed["type"].(string)
			if !ok {
				continue
			}
			pt := PacketType(ptStr)
			switch pt {
			case Pong:
				fmt.Println("[Server]: PONG received")
			case Message:
				data, ok := parsed["data"].(string)
				if ok {
					fmt.Println("\n[Server]:", data)
				}
			case Ack:
				fid, ok1 := parsed["file_id"].(string)
				idxF, ok2 := parsed["chunk_index"].(float64)
				if ok1 && ok2 {
					idx := int(idxF)
					// notify tracker to remove pending
					c.packet.tracker.NotifyAck(fid, idx)
					c.mu.Lock()
					if idx >= 0 {
						c.fileAcked[fid]++
					}
					c.mu.Unlock()
					fmt.Printf("[CLIENT] Received ACK %s_%d\n", fid, idx)
				}
			case FileMetadata:
				// incoming file from server (act as receiver)
				fid, ok1 := parsed["file_id"].(string)
				totalF, ok2 := parsed["total_chunks"].(float64)
				sizeF, ok3 := parsed["file_size"].(float64)
				name, ok4 := parsed["file_name"].(string)
				if ok1 && ok2 && ok3 && ok4 {
					total := int(totalF)
					size := int64(sizeF)
					_, exists := c.receivers[fid]
					if !exists {
						c.receivers[fid] = &FileReceiver{
							fileName:    name,
							totalChunks: total,
							fileSize:    size,
							chunks:      make(map[int][]byte),
						}
					}
					fmt.Printf("[CLIENT] Received METADATA file=%s name=%s total=%d size=%d\n", fid, name, total, size)
					// always ack metadata
					c.sendAck(fid, -1)
				}
			case FileChunk:
				fid, ok1 := parsed["file_id"].(string)
				idxF, ok2 := parsed["chunk_index"].(float64)
				data, ok3 := parsed["data"].([]byte)
				if ok1 && ok2 && ok3 {
					idx := int(idxF)
					r, ok := c.receivers[fid]
					if ok {
						if _, has := r.chunks[idx]; !has {
							r.chunks[idx] = data
						}
						c.sendAck(fid, idx)
						fmt.Printf("[CLIENT] Stored chunk %d/%d for file %s (stored=%d)\n", idx, r.totalChunks, r.fileName, len(r.chunks))
						if len(r.chunks) == r.totalChunks {
							var fullData []byte
							for i := 0; i < r.totalChunks; i++ {
								if chunk, ok := r.chunks[i]; ok {
									fullData = append(fullData, chunk...)
								} else {
									fmt.Printf("Missing chunk %d for %s\n", i, r.fileName)
									break
								}
							}
							if int64(len(fullData)) == r.fileSize {
								if err := os.WriteFile(r.fileName, fullData, 0644); err == nil {
									fmt.Println("File received and saved:", r.fileName)
								} else {
									fmt.Println("Error saving file:", err)
								}
							} else {
								fmt.Printf("File size mismatch for %s (got %d expected %d)\n", r.fileName, len(fullData), r.fileSize)
							}
							delete(c.receivers, fid)
						}
					}
				}
			}
		}
	}()

	// keepalive ping
	go func() {
		ticker := time.NewTicker(28 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			pingPacket := c.packet.Generate(Ping, nil)
			if err := c.comm.Send(c.serverAddr, pingPacket); err != nil {
				fmt.Println("Error sending PING:", err)
			} else {
				fmt.Println("Sent PING to server")
			}
		}
	}()

	// console input & commands (including sendfile)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(">> ")
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)
		if text == "exit" {
			fmt.Println("Client exiting...")
			break
		}
		// sendfile command: sendfile <path>
		if strings.HasPrefix(text, "sendfile ") {
			parts := strings.SplitN(text, " ", 2)
			if len(parts) < 2 {
				fmt.Println("Usage: sendfile <filePath>")
				continue
			}
			path := strings.TrimSpace(parts[1])
			if err := c.SendFile(path); err != nil {
				fmt.Println("Error sending file:", err)
			} else {
				fmt.Println("SendFile finished (or timed out according to attempts).")
			}
			continue
		}
		// normal message
		payload := map[string]interface{}{"data": text}
		packet := c.packet.Generate(Message, payload)
		if err := c.comm.Send(c.serverAddr, packet); err != nil {
			fmt.Println("Error writing to server:", err)
		}
	}
}

// SendFile: client-side sender (mirrors server SendFile)
func (c *UDPClient) SendFile(filePath string) error {
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
	metaPacket := c.packet.Generate(FileMetadata, metaPayload)

	// init progress
	c.mu.Lock()
	c.fileTotal[fileID] = totalChunks
	c.fileEnqueued[fileID] = 0
	c.fileAcked[fileID] = 0
	c.mu.Unlock()

	fmt.Printf("[CLIENT-SEND] Sending METADATA %s (chunks=%d size=%d)\n", fileID, totalChunks, fileSize)

	// Add metadata and wait for ACK
	if err := c.packet.tracker.AddAndWaitAck(fileID, -1, metaPacket, c.serverAddr, timeoutDuration); err != nil {
		return fmt.Errorf("metadata ack timeout: %w", err)
	}

	fmt.Printf("[CLIENT-SEND] METADATA ACK received for %s\n", fileID)

	// open and send chunks
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
		packet := c.packet.Generate(FileChunk, chunkPayload)
		c.packet.tracker.Add(fileID, idx, packet, c.serverAddr)
		c.mu.Lock()
		c.fileEnqueued[fileID]++
		enq := c.fileEnqueued[fileID]
		tot := c.fileTotal[fileID]
		c.mu.Unlock()
		fmt.Printf("[CLIENT-SEND] Enqueued chunk %d/%d for file %s\n", enq, tot, fileID)
	}

	// wait for completion (until tracker clears pending for file)
	start := time.Now()
	for time.Since(start) < timeoutDuration {
		if !c.packet.tracker.HasAnyPending(fileID) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if c.packet.tracker.HasAnyPending(fileID) {
		return fmt.Errorf("timeout waiting for file %s completion", fileID)
	}
	fmt.Printf("[CLIENT-SEND] File %s completed (acked %d/%d)\n", fileID, c.fileAcked[fileID], c.fileTotal[fileID])
	return nil
}

func main() {
	client, err := NewUDPClient("173.208.144.109") // عدّل العنوان حسب السيرفر
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer client.conn.Close()
	client.Start()
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
