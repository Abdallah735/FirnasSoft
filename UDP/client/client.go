// UDP client
// client.go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type UDPClient struct {
	serverAddr *net.UDPAddr
	conn       *net.UDPConn
	recvCh     chan []byte
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

	client := &UDPClient{serverAddr: addr, conn: conn, recvCh: make(chan []byte, 100)}
	// start receiver loop to push raw messages to recvCh
	go client.readLoop()
	return client, nil
}

func (c *UDPClient) readLoop() {
	buffer := make([]byte, 70000)
	for {
		n, _, err := c.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading from server:", err)
			continue
		}
		data := make([]byte, n)
		copy(data, buffer[:n])
		// push raw bytes to channel; helper sendFile will parse JSON acks
		select {
		case c.recvCh <- data:
		default:
			// drop if channel full
		}
	}
}

func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")
	fmt.Println("To send file: /sendfile <path>")

	go func() {
		for raw := range c.recvCh {
			// try parse JSON
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if t, ok := m["type"].(string); ok {
				switch t {
				case "ack":
					// print or ignore (SendFile listens on recvCh too)
					// fmt.Println("ACK", m)
				default:
					// print other server msgs
					fmt.Println("\n[Server RAW]:", string(raw))
					fmt.Print(">> ")
				}
			} else {
				fmt.Println("\n[Server]:", string(raw))
				fmt.Print(">> ")
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
		if strings.HasPrefix(text, "/sendfile ") {
			path := strings.TrimSpace(strings.TrimPrefix(text, "/sendfile "))
			if err := c.SendFile(path); err != nil {
				fmt.Println("SendFile error:", err)
			}
			continue
		}

		_, err := c.conn.Write([]byte(text))
		if err != nil {
			fmt.Println("Error writing to server:", err)
		}
	}
}

// SendFile: split file to chunks, send meta, wait meta_ack, send chunks with retries on lack of ack
func (c *UDPClient) SendFile(path string) error {
	const chunkSize = 65000
	const ackTimeout = 3 * time.Second
	const maxRetries = 5

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return err
	}
	totalSize := fi.Size()
	filename := filepath.Base(path)
	totalChunks := int((totalSize + int64(chunkSize) - 1) / int64(chunkSize))

	// 1) send meta
	meta := map[string]any{
		"type":     "meta",
		"filename": filename,
		"size":     totalSize,
	}
	metaB, _ := json.Marshal(meta)
	if _, err := c.conn.Write(append(metaB, '\n', '\n')); err != nil {
		return err
	}

	// wait for meta_ack
	fileID := ""
	metaAckDeadline := time.Now().Add(5 * time.Second)
WaitMeta:
	for time.Now().Before(metaAckDeadline) {
		select {
		case raw := <-c.recvCh:
			var resp map[string]any
			_ = json.Unmarshal(raw, &resp)
			if resp["type"] == "meta_ack" {
				if id, ok := resp["file_id"].(string); ok {
					fileID = id
					break WaitMeta
				}
			}
		case <-time.After(200 * time.Millisecond):
			// continue waiting
		}
	}
	if fileID == "" {
		return fmt.Errorf("no meta_ack received from server")
	}
	fmt.Println("Received file_id:", fileID, "totalChunks:", totalChunks)

	// Prepare a map to track acked sequences
	acked := make(map[int]bool)

	// read and send chunks sequentially (could be parallelized but keep simple)
	for seq := 0; seq < totalChunks; seq++ {
		offset := int64(seq) * int64(chunkSize)
		remain := totalSize - offset
		toRead := chunkSize
		if remain < int64(chunkSize) {
			toRead = int(remain)
		}
		buf := make([]byte, toRead)
		_, err := file.ReadAt(buf, offset)
		if err != nil && err.Error() != "EOF" {
			return err
		}

		// prepare header
		hdr := map[string]any{
			"type":    "chunk",
			"file_id": fileID,
			"seq":     seq,
			"offset":  offset,
			"size":    len(buf),
		}
		hdrB, _ := json.Marshal(hdr)
		packet := append(hdrB, []byte("\n\n")...)
		packet = append(packet, buf...)

		// send with retries until ACK
		sent := false
		for attempt := 0; attempt < maxRetries && !sent; attempt++ {
			_, err := c.conn.Write(packet)
			if err != nil {
				return err
			}

			// wait for ack for this seq
			ackWait := time.NewTimer(ackTimeout)
			ackReceived := false
		ACK_LOOP:
			for {
				select {
				case raw := <-c.recvCh:
					var resp map[string]any
					_ = json.Unmarshal(raw, &resp)
					if resp["type"] == "ack" {
						if fid, ok := resp["file_id"].(string); ok && fid == fileID {
							if seqF, ok := resp["seq"].(float64); ok {
								if int(seqF) == seq {
									acked[seq] = true
									ackReceived = true
									ackWait.Stop()
									break ACK_LOOP
								}
							}
						}
					}
				case <-ackWait.C:
					// timeout
					break ACK_LOOP
				}
			}
			if ackReceived {
				sent = true
			} else {
				// retry
				fmt.Printf("Retry chunk seq=%d attempt=%d\n", seq, attempt+1)
			}
		}
		if !sent {
			return fmt.Errorf("failed to send chunk %d after %d attempts", seq, maxRetries)
		}
		// small sleep to avoid flooding
		time.Sleep(5 * time.Millisecond)
	}

	fmt.Println("File sent successfully")
	return nil
}

func main() {
	client, err := NewUDPClient("127.0.0.1:10000") // replace with server ip if needed
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
