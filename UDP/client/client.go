// UDP client
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

type AckMsg struct {
	Type   string `json:"Type"`
	FileID string `json:"FileID,omitempty"`
	Seq    int    `json:"Seq,omitempty"`
	Path   string `json:"Path,omitempty"`
	Error  string `json:"Error,omitempty"`
}

type registerReq struct {
	Key string
	Ch  chan AckMsg
}

func NewUDPClient(server string) (*net.UDPConn, *net.UDPAddr, error) {
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, nil, fmt.Errorf("error resolving server address: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("error dialing server: %v", err)
	}

	return conn, addr, nil
}

func startAckDispatcher(incoming chan AckMsg, reg chan registerReq) {
	// owned map inside dispatcher goroutine
	waiters := make(map[string]chan AckMsg)
	for {
		select {
		case r := <-reg:
			waiters[r.Key] = r.Ch
		case ack := <-incoming:
			key := ack.FileID + "#" + fmt.Sprint(ack.Seq)
			if ch, ok := waiters[key]; ok {
				// deliver and remove waiter
				ch <- ack
				delete(waiters, key)
			} else {
				// maybe a FILEID or COMPLETE message (no seq)
				if ack.Type == "FILEID" || ack.Type == "COMPLETE" || ack.Type == "ERROR" {
					key2 := ack.FileID + "#-1"
					if ch2, ok2 := waiters[key2]; ok2 {
						ch2 <- ack
						delete(waiters, key2)
					}
				}
				// otherwise ignore or log
			}
		}
	}
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: client <server:port> <file-to-send>")
		return
	}
	server := os.Args[1]
	filePath := os.Args[2]

	conn, serverAddr, err := NewUDPClient(server)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer conn.Close()

	fmt.Println("UDP client started. Connected to", serverAddr.String())

	// reader goroutine: parse incoming JSON messages and push to incoming channel
	incoming := make(chan AckMsg, 100)
	reg := make(chan registerReq, 100)
	go startAckDispatcher(incoming, reg)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				fmt.Println("Read error:", err)
				return
			}
			var a AckMsg
			if err := json.Unmarshal(buf[:n], &a); err == nil {
				incoming <- a
			} else {
				fmt.Println("server:", string(buf[:n]))
			}
		}
	}()

	// read file
	finfo, err := os.Stat(filePath)
	if err != nil {
		fmt.Println("file stat error:", err)
		return
	}
	size := finfo.Size()
	fileName := filepath.Base(filePath)

	const chunkSize = 60 * 1024 // 60KB safe chunk
	totalParts := int((size + int64(chunkSize) - 1) / int64(chunkSize))

	f, err := os.Open(filePath)
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer f.Close()

	// read and send first chunk with metadata so server will generate fileID
	buf := make([]byte, chunkSize)
	n, _ := f.Read(buf)
	firstChunk := buf[:n]

	hdr := map[string]any{
		"Type":      "FIRST",
		"FileName":  fileName,
		"FileSize":  size,
		"Seq":       0,
		"ChunkSize": chunkSize,
	}
	hdrB, _ := json.Marshal(hdr)
	packet := append(hdrB, '\n')
	packet = append(packet, firstChunk...)

	// register a waiter for FILEID (key fileid#-1)
	waitKeyCh := make(chan AckMsg, 1)
	// use a temporary key "-1" until server returns fileID; dispatcher will match FileID#-1 later
	reg <- registerReq{Key: "-1", Ch: waitKeyCh}

	_, err = conn.Write(packet)
	if err != nil {
		fmt.Println("write error:", err)
		return
	}

	// wait for FILEID response
	var fileID string
	select {
	case a := <-waitKeyCh:
		if a.Type == "FILEID" {
			fileID = a.FileID
			fmt.Println("Received FileID from server:", fileID)
		} else if a.Type == "ERROR" {
			fmt.Println("Server error:", a.Error)
			return
		} else {
			fmt.Println("Unexpected server response:", a)
			return
		}
	case <-time.After(5 * time.Second):
		fmt.Println("timeout waiting for fileID")
		return
	}

	// Now we'll send remaining parts (if any). For simplicity resending logic is here:
	// for each seq: send -> register (fileID#seq) -> wait for ACK with timeout -> if timeout retry up to retries
	retries := 5

	// we've already sent seq 0; if more parts exist send them
	for seq := 1; seq < totalParts; seq++ {
		offset := int64(seq) * int64(chunkSize)
		_, err := f.Seek(offset, 0)
		if err != nil {
			fmt.Println("seek error:", err)
			return
		}
		n, _ := f.Read(buf)
		chunk := buf[:n]

		hdr := map[string]any{
			"Type":   "CHUNK",
			"FileID": fileID,
			"Seq":    seq,
		}
		hdrB, _ := json.Marshal(hdr)
		packet := append(hdrB, '\n')
		packet = append(packet, chunk...)

		key := fileID + "#" + fmt.Sprint(seq)

		sent := false
		for attempt := 0; attempt < retries && !sent; attempt++ {
			waitCh := make(chan AckMsg, 1)
			reg <- registerReq{Key: key, Ch: waitCh}

			_, err := conn.Write(packet)
			if err != nil {
				fmt.Println("write error:", err)
				return
			}

			select {
			case a := <-waitCh:
				if a.Type == "ACK" && a.Seq == seq {
					// ok
					fmt.Printf("ACK for seq %d\n", seq)
					sent = true
					break
				} else if a.Type == "ERROR" {
					fmt.Println("server error:", a.Error)
					// decide: retry or abort
				}
			case <-time.After(2 * time.Second):
				fmt.Printf("timeout waiting ACK for seq %d (attempt %d)\n", seq, attempt+1)
				// loop to retry
			}
		}
		if !sent {
			fmt.Printf("failed to send seq %d after %d attempts\n", seq, retries)
			return
		}
	}

	// wait for COMPLETE
	completeKey := fileID + "#-1"
	completeCh := make(chan AckMsg, 1)
	reg <- registerReq{Key: completeKey, Ch: completeCh}

	select {
	case a := <-completeCh:
		if a.Type == "COMPLETE" {
			fmt.Println("Server reports file stored at:", a.Path)
		} else {
			fmt.Println("Got message:", a)
		}
	case <-time.After(10 * time.Second):
		fmt.Println("timeout waiting for COMPLETE")
	}
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
