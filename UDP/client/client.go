// UDP client
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
	conn       *net.UDPConn // main connection (for chat)
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
	return &UDPClient{serverAddr: addr, conn: conn}, nil
}

// ackRouter types
type ackReg struct {
	seq int
	ch  chan struct{}
}
type ackMsg struct {
	fileID string
	seq    int
}

// SendFile: sequential send of meta (wait for ack), then parallel sending of rest using worker pool
func (c *UDPClient) SendFile(path string) error {
	const chunkSize = 65000
	const ackTimeout = 5 * time.Second
	const maxRetries = 6
	poolSize := 6 // tune this (عدد الجو روتين العاملة)

	// open file
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	totalSize := fi.Size()
	numChunks := int((totalSize + int64(chunkSize) - 1) / int64(chunkSize))
	if numChunks == 0 {
		return fmt.Errorf("empty file")
	}

	// create a dedicated UDP connection for this transfer (all chunks must come from the same local port)
	localConn, err := net.DialUDP("udp", nil, c.serverAddr)
	if err != nil {
		return err
	}
	defer localConn.Close()

	// --- send first chunk (metadata) and wait for ack to get file_id ---
	// read first chunk payload
	firstSize := chunkSize
	if int64(firstSize) > totalSize {
		firstSize = int(totalSize)
	}
	firstBuf := make([]byte, firstSize)
	_, _ = f.ReadAt(firstBuf, 0)

	metaHeader := map[string]any{
		"first":      true,
		"seq":        0,
		"chunk_size": chunkSize,
		"total_size": totalSize,
		"file_name":  filepath.Base(path),
	}
	hb, _ := json.Marshal(metaHeader)
	metaPacket := append(hb, '\n')
	metaPacket = append(metaPacket, firstBuf...)

	if _, err := localConn.Write(metaPacket); err != nil {
		return fmt.Errorf("error sending metadata: %v", err)
	}

	// wait for ack (blocking read for meta)
	metaBuf := make([]byte, 2048)
	_ = localConn.SetReadDeadline(time.Now().Add(ackTimeout))
	n, _, err := localConn.ReadFromUDP(metaBuf)
	if err != nil {
		return fmt.Errorf("no ack for metadata: %v", err)
	}
	var metaAck struct {
		Type   string `json:"type"`
		FileID string `json:"file_id"`
		Seq    int    `json:"seq"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(metaBuf[:n], &metaAck); err != nil {
		return fmt.Errorf("invalid ack for metadata: %v", err)
	}
	if metaAck.Type != "ack" || metaAck.FileID == "" {
		return fmt.Errorf("metadata not acknowledged properly: %s", string(metaBuf[:n]))
	}
	fileID := metaAck.FileID
	fmt.Println("Got file_id:", fileID)

	// --- start ack reader + router ---
	regCh := make(chan ackReg)
	incomingCh := make(chan ackMsg)

	// reader: reads incoming JSON ACKs and forwards to incomingCh
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := localConn.ReadFromUDP(buf)
			if err != nil {
				// when localConn is closed, this will error and goroutine stops
				close(incomingCh)
				return
			}
			var m map[string]any
			if err := json.Unmarshal(buf[:n], &m); err != nil {
				continue
			}
			t, _ := m["type"].(string)
			if t != "ack" {
				continue
			}
			seqf, ok := m["seq"].(float64)
			if !ok {
				continue
			}
			fid, _ := m["file_id"].(string)
			incomingCh <- ackMsg{fileID: fid, seq: int(seqf)}
		}
	}()

	// router: single goroutine that keeps a map[seq]chan and dispatches incoming acks
	go func() {
		waiters := make(map[int]chan struct{})
		for {
			select {
			case reg, ok := <-regCh:
				if !ok {
					// cleanup and exit
					for _, ch := range waiters {
						close(ch)
					}
					return
				}
				// register/overwrite
				waiters[reg.seq] = reg.ch
			case im, ok := <-incomingCh:
				if !ok {
					// incoming closed -> exit
					for _, ch := range waiters {
						close(ch)
					}
					return
				}
				// dispatch if waiter exists
				if ch, found := waiters[im.seq]; found {
					// notify and remove
					select {
					case ch <- struct{}{}:
					default:
					}
					delete(waiters, im.seq)
				}
			}
		}
	}()

	// --- worker pool to send chunks in parallel ---
	type task struct {
		seq int
	}
	tasks := make(chan task, numChunks)
	doneCh := make(chan int, numChunks)
	errCh := make(chan error, 1)

	// worker logic: register, send, wait for ack (with retries)
	for w := 0; w < poolSize; w++ {
		go func(workerID int) {
			for t := range tasks {
				seq := t.seq
				off := int64(seq) * int64(chunkSize)
				readSize := chunkSize
				if off+int64(readSize) > totalSize {
					readSize = int(totalSize - off)
				}
				buf := make([]byte, readSize)
				_, err := f.ReadAt(buf, off)
				if err != nil {
					// read error -> report and stop
					select {
					case errCh <- fmt.Errorf("read error seq %d: %v", seq, err):
					default:
					}
					return
				}

				// attempt with retries
				retries := 0
				for {
					ackWait := make(chan struct{}, 1)
					// register waiter
					regCh <- ackReg{seq: seq, ch: ackWait}

					// build header
					h := map[string]any{
						"first":      false,
						"seq":        seq,
						"chunk_size": chunkSize,
						"file_id":    fileID,
					}
					hj, _ := json.Marshal(h)
					pkt := append(hj, '\n')
					pkt = append(pkt, buf...)

					// write (concurrent writes are OK)
					if _, err := localConn.Write(pkt); err != nil {
						// immediate write error
						retries++
						if retries > maxRetries {
							select {
							case errCh <- fmt.Errorf("write failed seq %d: %v", seq, err):
							default:
							}
							return
						}
						time.Sleep(500 * time.Millisecond)
						continue
					}

					// wait ack or timeout
					select {
					case <-ackWait:
						// success
						doneCh <- seq
						goto nextTask
					case <-time.After(ackTimeout):
						retries++
						if retries > maxRetries {
							select {
							case errCh <- fmt.Errorf("no ack for seq %d after %d retries", seq, maxRetries):
							default:
							}
							return
						}
						// retry (loop)
						// re-register on next iteration
					}
				}
			nextTask:
				continue
			}
		}(w)
	}

	// enqueue tasks for seq 1..numChunks-1 because seq 0 already sent with meta
	for seq := 1; seq < numChunks; seq++ {
		tasks <- task{seq: seq}
	}
	close(tasks)

	// wait for completions
	expected := numChunks - 1 // since seq 0 done
	received := 0
	for received < expected {
		select {
		case <-doneCh:
			received++
			// optionally print progress
			if received%10 == 0 || received == expected {
				fmt.Printf("Progress: %d/%d chunks acknowledged\n", received, expected)
			}
		case e := <-errCh:
			// fatal error
			localConn.Close()
			return e
		}
	}

	// all done
	fmt.Println("File transfer complete")
	// close router by closing regCh and localConn (reader goroutine will exit)
	close(regCh)
	localConn.Close()
	return nil
}

func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, _, err := c.conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading from server:", err)
				continue
			}
			fmt.Println("\n[Server]:", string(buffer[:n]))
			fmt.Print(">> ")
		}
	}()

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
			parts := strings.SplitN(text, " ", 2)
			if len(parts) < 2 {
				fmt.Println("Usage: sendfile <path>")
				continue
			}
			if err := c.SendFile(strings.TrimSpace(parts[1])); err != nil {
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

func main() {
	client, err := NewUDPClient("173.208.144.109:10000")
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
// 	client, err := NewUDPClient("173.208.144.109:10000")
// 	if err != nil {
// 		fmt.Println("Error:", err)
// 		return
// 	}
// 	defer client.conn.Close()

// 	client.Start()
// }
