// UDP client
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type PacketHeader struct {
	Type        string `json:"type"` // INIT, CHUNK, ACK_INIT, ACK_CHUNK, ACK_COMPLETE
	Filename    string `json:"filename,omitempty"`
	Filesize    int64  `json:"filesize,omitempty"`
	ChunkSeq    int    `json:"seq,omitempty"`
	ChunkSize   int    `json:"chunk_size,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	TotalChunks int    `json:"total_chunks,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type UDPClient struct {
	serverAddr *net.UDPAddr
	conn       *net.UDPConn
	ackCh      chan PacketHeader // channel to receive ACK-like headers
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

	c := &UDPClient{
		serverAddr: addr,
		conn:       conn,
		ackCh:      make(chan PacketHeader, 100),
	}

	// reader goroutine (routes ACK_* messages to ackCh, others print)
	go c.readerLoop()

	return c, nil
}

func (c *UDPClient) readerLoop() {
	buffer := make([]byte, 65536)
	for {
		n, _, err := c.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading from server:", err)
			time.Sleep(time.Second)
			continue
		}
		data := make([]byte, n)
		copy(data, buffer[:n])

		// try parse header-only JSON
		var hdr PacketHeader
		if err := json.Unmarshal(data, &hdr); err == nil {
			// route ACKs to ackCh
			if strings.HasPrefix(hdr.Type, "ACK") {
				select {
				case c.ackCh <- hdr:
				default:
					// if channel full, drop to avoid blocking
				}
				continue
			}
			// otherwise print general messages
			fmt.Println("\n[Server JSON]:", string(data))
			fmt.Print(">> ")
			continue
		}

		// if not JSON, print raw
		fmt.Println("\n[Server]:", string(data))
		fmt.Print(">> ")
	}
}

// helper: send header + optional payload (header JSON + '\n' + payload)
func (c *UDPClient) sendPacketWithPayload(h PacketHeader, payload []byte) error {
	hb, _ := json.Marshal(h)
	if payload == nil {
		_, err := c.conn.Write(hb)
		return err
	}
	packet := make([]byte, 0, len(hb)+1+len(payload))
	packet = append(packet, hb...)
	packet = append(packet, '\n')
	packet = append(packet, payload...)
	_, err := c.conn.Write(packet)
	return err
}

// SendFile implements send with INIT and chunk retry logic
func (c *UDPClient) SendFile(path string, chunkSize int, timeout time.Duration, maxRetries int) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file: %v", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat file: %v", err)
	}
	filesize := stat.Size()
	totalChunks := int(math.Ceil(float64(filesize) / float64(chunkSize)))
	filename := filepath.Base(path)

	// 1) send INIT
	initHdr := PacketHeader{
		Type:        "INIT",
		Filename:    filename,
		Filesize:    filesize,
		ChunkSize:   chunkSize,
		TotalChunks: totalChunks,
	}
	if err := c.sendPacketWithPayload(initHdr, nil); err != nil {
		return fmt.Errorf("send INIT: %v", err)
	}

	// wait for ACK_INIT
	var fileID string
WAIT_INIT_ACK:
	for {
		select {
		case ack := <-c.ackCh:
			if ack.Type == "ACK_INIT" && ack.FileID != "" {
				fileID = ack.FileID
				break WAIT_INIT_ACK
			} else if ack.Type == "ACK_INIT" && ack.FileID == "" {
				return fmt.Errorf("server rejected INIT: %s", ack.Reason)
			}
			// ignore other ACKs
		case <-time.After(timeout):
			// retry INIT
			fmt.Println("No ACK_INIT, retrying INIT...")
			if err := c.sendPacketWithPayload(initHdr, nil); err != nil {
				return fmt.Errorf("send INIT retry: %v", err)
			}
		}
	}
	fmt.Println("Received fileID:", fileID)

	// 2) send chunks sequentially with ack & retries
	buf := make([]byte, chunkSize)
	for seq := 0; seq < totalChunks; seq++ {
		off := int64(seq) * int64(chunkSize)
		n, err := f.ReadAt(buf, off)
		if err != nil && n == 0 {
			// EOF or error with no bytes
			return fmt.Errorf("read chunk %d: %v", seq, err)
		}
		chunkData := make([]byte, n)
		copy(chunkData, buf[:n])

		chdr := PacketHeader{
			Type:      "CHUNK",
			FileID:    fileID,
			ChunkSeq:  seq,
			ChunkSize: chunkSize,
		}

		retries := 0
	SEND_CHUNK:
		if err := c.sendPacketWithPayload(chdr, chunkData); err != nil {
			return fmt.Errorf("send chunk %d: %v", seq, err)
		}

		// wait for ACK_CHUNK
	ACK_WAIT:
		for {
			select {
			case ack := <-c.ackCh:
				if ack.Type == "ACK_CHUNK" && ack.FileID == fileID && ack.ChunkSeq == seq {
					// chunk confirmed
					//fmt.Printf("ACK chunk %d\n", seq)
					break ACK_WAIT
				} else if ack.Type == "ACK_CHUNK" && ack.FileID == fileID && ack.ChunkSeq == seq && ack.Reason != "" {
					// server reported issue
					return fmt.Errorf("server NAK chunk %d: %s", seq, ack.Reason)
				} else if ack.Type == "ACK_COMPLETE" && ack.FileID == fileID {
					// server says complete early
					fmt.Println("Server reported complete for file:", fileID)
					return nil
				}
				// else ignore and keep waiting
			case <-time.After(timeout):
				retries++
				if retries > maxRetries {
					return fmt.Errorf("chunk %d: no ack after %d retries", seq, retries-1)
				}
				fmt.Printf("Timeout waiting ACK for chunk %d, retry %d/%d\n", seq, retries, maxRetries)
				goto SEND_CHUNK
			}
		}
	}

	// optionally wait for ACK_COMPLETE
	waitCompleteTimer := time.NewTimer(5 * time.Second)
	defer waitCompleteTimer.Stop()
	for {
		select {
		case ack := <-c.ackCh:
			if ack.Type == "ACK_COMPLETE" && ack.FileID == fileID {
				fmt.Println("File transfer complete!")
				return nil
			}
		case <-waitCompleteTimer.C:
			fmt.Println("No ACK_COMPLETE received, assuming done.")
			return nil
		}
	}
}

// Start interactive client
func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")
	fmt.Println("To send file: sendfile <path> [chunkSize]")
	// keepalive ticker
	go func() {
		ticker := time.NewTicker(28 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_, err := c.conn.Write([]byte("PING"))
			if err != nil {
				fmt.Println("Error sending keep-alive msg:", err)
			}
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(">> ")
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)

		if text == "exit" {
			fmt.Println("Client exiting...")
			break
		}
		if strings.HasPrefix(text, "sendfile ") {
			parts := strings.SplitN(text, " ", 3)
			path := strings.TrimSpace(parts[1])
			chunkSize := 65000 // default ~65KB
			if len(parts) == 3 {
				// try parse chunk size
				fmt.Sscanf(parts[2], "%d", &chunkSize)
			}
			fmt.Println("Sending file:", path, "chunkSize:", chunkSize)
			if err := c.SendFile(path, chunkSize, 5*time.Second, 5); err != nil {
				fmt.Println("SendFile error:", err)
			} else {
				fmt.Println("SendFile finished.")
			}
			continue
		}

		_, err := c.conn.Write([]byte(text))
		if err != nil {
			fmt.Println("Error writing to server:", err)
		}
	}
}

func main() {
	client, err := NewUDPClient("173.208.144.109:10000") // replace with server ip if needed
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer client.conn.Close()

	client.Start()
}

//------------------------------------------
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
// 	client, err := NewUDPClient("173.208.144.109:10000")
// 	if err != nil {
// 		fmt.Println("Error:", err)
// 		return
// 	}
// 	defer client.conn.Close()

// 	client.Start()
// }
