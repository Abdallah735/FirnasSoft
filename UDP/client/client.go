package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
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

// const (
// 	chunkSize = 32 * 1024 // For consistency with server, though not used in client for splitting.
// )

type FileReceiver struct {
	fileName    string
	totalChunks int
	fileSize    int64
	chunks      map[int][]byte
}

type UDPClient struct {
	serverAddr *net.UDPAddr
	conn       *net.UDPConn
	receivers  map[string]*FileReceiver
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

	return &UDPClient{
		serverAddr: addr,
		conn:       conn,
		receivers:  make(map[string]*FileReceiver),
	}, nil
}

func generate(pt PacketType, payload map[string]interface{}) []byte {
	m := map[string]interface{}{"type": string(pt)}
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

func parse(data []byte) (map[string]interface{}, error) {
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

func (c *UDPClient) sendAck(fileID string, chunkIndex int) {
	ackPayload := map[string]interface{}{"file_id": fileID, "chunk_index": chunkIndex}
	ackPacket := generate(Ack, ackPayload)
	_, err := c.conn.Write(ackPacket)
	if err != nil {
		fmt.Println("Error sending ack:", err)
	}
}

func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")

	go func() {
		buffer := make([]byte, 65536)
		for {
			n, _, err := c.conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading from server:", err)
				continue
			}
			dataCopy := make([]byte, n)
			copy(dataCopy, buffer[:n])
			parsed, err := parse(dataCopy)
			if err != nil {
				fmt.Printf("Parse error (len %d): %v\n", n, err) // Added logging for debug.
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
					fmt.Print(">> ")
				}
			case FileMetadata:
				fileID, ok1 := parsed["file_id"].(string)
				totalF, ok2 := parsed["total_chunks"].(float64)
				sizeF, ok3 := parsed["file_size"].(float64)
				name, ok4 := parsed["file_name"].(string)
				if ok1 && ok2 && ok3 && ok4 {
					total := int(totalF)
					size := int64(sizeF)
					_, exists := c.receivers[fileID]
					if !exists {
						c.receivers[fileID] = &FileReceiver{
							fileName:    name,
							totalChunks: total,
							fileSize:    size,
							chunks:      make(map[int][]byte),
						}
					}
					// Always ack, even on duplicate.
					c.sendAck(fileID, -1)
				}
			case FileChunk:
				fileID, ok1 := parsed["file_id"].(string)
				idxF, ok2 := parsed["chunk_index"].(float64)
				data, ok3 := parsed["data"].([]byte)
				if ok1 && ok2 && ok3 {
					idx := int(idxF)
					r, ok := c.receivers[fileID]
					if ok {
						// Set only if new; always ack.
						if _, has := r.chunks[idx]; !has {
							r.chunks[idx] = data
						}
						c.sendAck(fileID, idx)
						// Check len(chunks) instead of counter.
						if len(r.chunks) == r.totalChunks {
							var fullData []byte
							for i := 0; i < r.totalChunks; i++ {
								chunk, ok := r.chunks[i]
								if !ok {
									fmt.Printf("Missing chunk %d for %s\n", i, r.fileName)
									break
								}
								fullData = append(fullData, chunk...)
							}
							if int64(len(fullData)) != r.fileSize {
								fmt.Printf("File size mismatch for %s (got %d, expected %d)\n", r.fileName, len(fullData), r.fileSize)
							} else {
								err := os.WriteFile(r.fileName, fullData, 0644)
								if err != nil {
									fmt.Println("Error saving file:", err)
								} else {
									fmt.Println("File received and saved:", r.fileName)
								}
							}
							delete(c.receivers, fileID)
						}
					}
				}
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(28 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			pingPacket := generate(Ping, nil)
			_, err := c.conn.Write(pingPacket)
			if err != nil {
				fmt.Println("Error sending PING:", err)
			} else {
				fmt.Println("Sent PING to server")
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

		payload := map[string]interface{}{"data": text}
		packet := generate(Message, payload)
		_, err := c.conn.Write(packet)
		if err != nil {
			fmt.Println("Error writing to server:", err)
		}
	}
}

func main() {
	client, err := NewUDPClient("173.208.144.109:10000") //173.208.144.109  //127.0.0.1
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
