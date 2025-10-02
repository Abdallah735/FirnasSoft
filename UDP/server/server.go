// UDP server
// server.go
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

	"github.com/google/uuid"
)

type commandType int

const (
	addClient commandType = iota
	sendMessage
	listClients
	getClient
	packet // new: raw packet arrived (data + addr)
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error
}

// File tracking structures inside manager
type fileMeta struct {
	FileID      string
	Owner       string // client addr string
	Filename    string
	TotalSize   int64
	ChunkSize   int
	TotalChunks int
	Received    map[int]bool
	ReceivedCnt int
	FilePath    string
	File        *os.File
	CreatedAt   time.Time
}

type packetPayload struct {
	data []byte
	addr *net.UDPAddr
}

// ----------------- Server Struct -----------------
type Server struct {
	addr     string
	conn     *net.UDPConn
	commands chan *command
}

func NewServer(addr string) *Server {
	return &Server{
		addr:     addr,
		commands: make(chan *command),
	}
}

func (s *Server) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		return fmt.Errorf("error resolving address: %v", err)
	}

	s.conn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("error listening: %v", err)
	}

	fmt.Println("UDP server listening on", s.addr)

	// Start manager
	go s.clientManagerWorker()

	// Start console input (unchanged)
	go s.handleInput()

	// Start handling packets
	s.handlePackets()

	return nil
}

// manager worker: now holds clients map and files map; processes packet commands
func (s *Server) clientManagerWorker() {
	clients := make(map[string]*net.UDPAddr)
	files := make(map[string]*fileMeta)

	chunkSize := 65000 // same used by client

	for {
		cmd := <-s.commands

		switch cmd.typ {
		case addClient:
			clients[cmd.addr.String()] = cmd.addr
			cmd.errCh <- nil

		case sendMessage:
			if client, ok := clients[cmd.targetKey]; ok {
				var data []byte
				switch v := cmd.message.(type) {
				case string:
					data = []byte(v)
				case []byte:
					data = v
				default:
					jsonData, err := json.Marshal(v)
					if err != nil {
						cmd.errCh <- fmt.Errorf("unsupported message type: %T", v)
						continue
					}
					data = jsonData
				}

				n, err := s.conn.WriteToUDP(data, client)
				if err != nil {
					cmd.errCh <- err
					continue
				}
				if n != len(data) {
					cmd.errCh <- fmt.Errorf("incomplete write: wrote %d of %d", n, len(data))
					continue
				}
				cmd.errCh <- nil
			} else {
				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
			}

		case listClients:
			list := make([]string, 0, len(clients))
			for k := range clients {
				list = append(list, k)
			}
			cmd.replyCh <- list

		case getClient:
			if client, ok := clients[cmd.targetKey]; ok {
				cmd.replyCh <- client
			} else {
				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
			}

		case packet:
			// message is packetPayload
			pp := cmd.message.(packetPayload)
			data := pp.data
			addr := pp.addr

			// try split header \n\n
			splitIdx := -1
			for i := 0; i < len(data)-1; i++ {
				// find first occurrence of "\n\n"
				if data[i] == '\n' && data[i+1] == '\n' {
					splitIdx = i
					break
				}
			}

			var headerData []byte
			var body []byte
			if splitIdx != -1 {
				headerData = data[:splitIdx]
				if splitIdx+2 < len(data) {
					body = data[splitIdx+2:]
				} else {
					body = []byte{}
				}
			} else {
				// no header separator: treat whole as text
				headerData = data
				body = []byte{}
			}

			var hdr map[string]any
			if err := json.Unmarshal(headerData, &hdr); err != nil {
				// not JSON header -> ignore or log
				fmt.Printf("Received non-json header from %s: %s\n", addr.String(), string(headerData))
				continue
			}

			typ, _ := hdr["type"].(string)
			switch typ {
			case "meta":
				// create file meta
				filename, _ := hdr["filename"].(string)
				sizeFloat, _ := hdr["size"].(float64)
				totalSize := int64(sizeFloat)

				// generate file id using UUID
				fileID := uuid.New().String()

				// file path
				tmpDir := "./received_files"
				_ = os.MkdirAll(tmpDir, 0755)
				filePath := filepath.Join(tmpDir, fileID+"_"+filepath.Base(filename))

				// create file and preallocate
				f, err := os.Create(filePath)
				if err != nil {
					fmt.Println("Error creating file:", err)
					continue
				}
				if err := f.Truncate(totalSize); err != nil {
					fmt.Println("Error truncating file:", err)
					f.Close()
					continue
				}

				totalChunks := int((totalSize + int64(chunkSize) - 1) / int64(chunkSize))
				meta := &fileMeta{
					FileID:      fileID,
					Owner:       addr.String(),
					Filename:    filename,
					TotalSize:   totalSize,
					ChunkSize:   chunkSize,
					TotalChunks: totalChunks,
					Received:    make(map[int]bool),
					ReceivedCnt: 0,
					FilePath:    filePath,
					File:        f,
					CreatedAt:   time.Now(),
				}
				files[fileID] = meta

				// respond with meta_ack and file_id
				resp := map[string]any{
					"type":    "meta_ack",
					"file_id": fileID,
				}
				b, _ := json.Marshal(resp)
				_, _ = s.conn.WriteToUDP(append(b, '\n'), addr)
				fmt.Printf("Meta received from %s: file=%s size=%d chunks=%d id=%s\n",
					addr.String(), filename, totalSize, totalChunks, fileID)

			case "chunk":
				// expected fields: file_id, seq (int), offset (float64), size (float64)
				fileID, _ := hdr["file_id"].(string)
				seqFloat, _ := hdr["seq"].(float64)
				seq := int(seqFloat)
				offsetF, _ := hdr["offset"].(float64)
				offset := int64(offsetF)
				sizeF, _ := hdr["size"].(float64)
				chunkLen := int(sizeF)

				meta, ok := files[fileID]
				if !ok {
					// unknown file -> reply error
					resp := map[string]any{
						"type":    "nack",
						"file_id": fileID,
						"seq":     seq,
						"reason":  "unknown_file",
					}
					b, _ := json.Marshal(resp)
					_, _ = s.conn.WriteToUDP(append(b, '\n'), addr)
					fmt.Printf("Received chunk for unknown file %s from %s\n", fileID, addr.String())
					continue
				}
				// check owner matches
				if meta.Owner != addr.String() {
					// ignore or send nack
					resp := map[string]any{
						"type":    "nack",
						"file_id": fileID,
						"seq":     seq,
						"reason":  "wrong_owner",
					}
					b, _ := json.Marshal(resp)
					_, _ = s.conn.WriteToUDP(append(b, '\n'), addr)
					fmt.Printf("Chunk owner mismatch: %s vs %s\n", addr.String(), meta.Owner)
					continue
				}

				// write to file at offset
				if len(body) != chunkLen {
					// maybe truncated packet
					fmt.Printf("Chunk len mismatch for %s seq %d: header size=%d body=%d\n",
						fileID, seq, chunkLen, len(body))
				}

				_, err := meta.File.WriteAt(body, offset)
				if err != nil {
					fmt.Println("Error writing chunk:", err)
					// send nack
					resp := map[string]any{
						"type":    "nack",
						"file_id": fileID,
						"seq":     seq,
						"reason":  "write_error",
					}
					b, _ := json.Marshal(resp)
					_, _ = s.conn.WriteToUDP(append(b, '\n'), addr)
					continue
				}

				// mark received if not already
				if !meta.Received[seq] {
					meta.Received[seq] = true
					meta.ReceivedCnt++
				}

				// send ack
				ack := map[string]any{
					"type":    "ack",
					"file_id": fileID,
					"seq":     seq,
				}
				ackB, _ := json.Marshal(ack)
				_, _ = s.conn.WriteToUDP(append(ackB, '\n'), addr)

				// if done -> finalize
				if meta.ReceivedCnt >= meta.TotalChunks {
					meta.File.Close()
					fmt.Printf("Received complete file %s from %s -> saved to %s\n",
						meta.Filename, addr.String(), meta.FilePath)
					delete(files, fileID)
				}

			default:
				fmt.Printf("Unknown packet type from %s: %s\n", addr.String(), typ)
			}
		}
	}
}

// Send message (unchanged)
func (s *Server) SendMessage(target string, msg any) error {
	errCh := make(chan error)
	s.commands <- &command{
		typ:       sendMessage,
		targetKey: target,
		message:   msg,
		errCh:     errCh,
	}
	return <-errCh
}

func (s *Server) ListClients() ([]string, error) {
	replyCh := make(chan any)
	errCh := make(chan error)

	s.commands <- &command{
		typ:     listClients,
		replyCh: replyCh,
		errCh:   errCh,
	}

	select {
	case data := <-replyCh:
		return data.([]string), nil
	case err := <-errCh:
		return nil, err
	}
}

func (s *Server) GetClient(addr string) (*net.UDPAddr, error) {
	replyCh := make(chan any)
	errCh := make(chan error)

	s.commands <- &command{
		typ:       getClient,
		targetKey: addr,
		replyCh:   replyCh,
		errCh:     errCh,
	}

	select {
	case data := <-replyCh:
		return data.(*net.UDPAddr), nil
	case err := <-errCh:
		return nil, err
	}
}

func (s *Server) AddClient(addr *net.UDPAddr) error {
	errCh := make(chan error)
	s.commands <- &command{
		typ:   addClient,
		addr:  addr,
		errCh: errCh,
	}
	return <-errCh
}

// Handle console input (unchanged)
func (s *Server) handleInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Server input> ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "send ") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 {
				fmt.Println("Usage: send <clientAddr> <message>")
				continue
			}
			if err := s.SendMessage(parts[1], "[Server] "+parts[2]); err != nil {
				fmt.Println("Error:", err)
			}
		} else if line == "list" {
			clients, err := s.ListClients()
			if err != nil {
				fmt.Println("Error:", err)
				continue
			}
			for _, c := range clients {
				fmt.Println("Client:", c)
			}
		}
	}
}

// Handle incoming packets: read and forward as packet command
func (s *Server) handlePackets() {
	buffer := make([]byte, 70000) // large buffer
	for {
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}

		// Register client
		if err := s.AddClient(clientAddr); err != nil {
			fmt.Println("Error adding client:", err)
		}

		// copy data to avoid overwrite
		dataCopy := make([]byte, n)
		copy(dataCopy, buffer[:n])

		// send packet command to manager
		s.commands <- &command{
			typ:     packet,
			message: packetPayload{data: dataCopy, addr: clientAddr},
		}
	}
}

func main() {
	server := NewServer(":10000")
	if err := server.Start(); err != nil {
		fmt.Println("Server error:", err)
	}
}

//------------------------------------------
// package main

// import (
// 	"bufio"
// 	"encoding/json"
// 	"fmt"
// 	"net"
// 	"os"
// 	"strings"
// )

// type commandType int

// const (
// 	addClient commandType = iota
// 	sendMessage
// 	listClients
// 	getClient
// )

// type command struct {
// 	typ       commandType
// 	addr      *net.UDPAddr
// 	targetKey string
// 	message   any
// 	replyCh   chan any
// 	errCh     chan error
// }

// // ----------------- Server Struct -----------------
// type Server struct {
// 	addr     string
// 	conn     *net.UDPConn
// 	commands chan *command
// }

// // Create new server
// func NewServer(addr string) *Server {
// 	return &Server{
// 		addr:     addr,
// 		commands: make(chan *command),
// 	}
// }

// // Start server
// func (s *Server) Start() error {
// 	udpAddr, err := net.ResolveUDPAddr("udp", s.addr)
// 	if err != nil {
// 		return fmt.Errorf("Error resolving address: %v", err)
// 	}

// 	s.conn, err = net.ListenUDP("udp", udpAddr)
// 	if err != nil {
// 		return fmt.Errorf("Error listening: %v", err)
// 	}

// 	fmt.Println("UDP server listening on", s.addr)

// 	// Start manager
// 	go s.clientManagerWorker()

// 	// Start console input
// 	go s.handleInput()

// 	// Start handling packets
// 	s.handlePackets()

// 	return nil
// }

// // Manager goroutine
// func (s *Server) clientManagerWorker() {
// 	clients := make(map[string]*net.UDPAddr)

// 	for {
// 		cmd := <-s.commands

// 		switch cmd.typ {
// 		case addClient:
// 			clients[cmd.addr.String()] = cmd.addr
// 			cmd.errCh <- nil

// 		case sendMessage:
// 			if client, ok := clients[cmd.targetKey]; ok {
// 				var data []byte
// 				switch v := cmd.message.(type) {
// 				case string:
// 					data = []byte(v)
// 				case []byte:
// 					data = v
// 				default:
// 					jsonData, err := json.Marshal(v)
// 					if err != nil {
// 						cmd.errCh <- fmt.Errorf("unsupported message type: %T", v)
// 						continue
// 					}
// 					data = jsonData
// 				}

// 				n, err := s.conn.WriteToUDP(data, client)
// 				if err != nil {
// 					cmd.errCh <- err
// 					continue
// 				}
// 				if n != len(data) {
// 					cmd.errCh <- fmt.Errorf("incomplete write: wrote %d of %d", n, len(data))
// 					continue
// 				}
// 				cmd.errCh <- nil
// 			} else {
// 				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
// 			}

// 		case listClients:
// 			list := make([]string, 0, len(clients))
// 			for k := range clients {
// 				list = append(list, k)
// 			}
// 			cmd.replyCh <- list

// 		case getClient:
// 			if client, ok := clients[cmd.targetKey]; ok {
// 				cmd.replyCh <- client
// 			} else {
// 				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
// 			}
// 		}
// 	}
// }

// // Send message
// func (s *Server) SendMessage(target string, msg any) error {
// 	errCh := make(chan error)
// 	s.commands <- &command{
// 		typ:       sendMessage,
// 		targetKey: target,
// 		message:   msg,
// 		errCh:     errCh,
// 	}
// 	return <-errCh
// }

// // List clients
// func (s *Server) ListClients() ([]string, error) {
// 	replyCh := make(chan any)
// 	errCh := make(chan error)

// 	s.commands <- &command{
// 		typ:     listClients,
// 		replyCh: replyCh,
// 		errCh:   errCh,
// 	}

// 	select {
// 	case data := <-replyCh:
// 		return data.([]string), nil
// 	case err := <-errCh:
// 		return nil, err
// 	}
// }

// // Get client
// func (s *Server) GetClient(addr string) (*net.UDPAddr, error) {
// 	replyCh := make(chan any)
// 	errCh := make(chan error)

// 	s.commands <- &command{
// 		typ:       getClient,
// 		targetKey: addr,
// 		replyCh:   replyCh,
// 		errCh:     errCh,
// 	}

// 	select {
// 	case data := <-replyCh:
// 		return data.(*net.UDPAddr), nil
// 	case err := <-errCh:
// 		return nil, err
// 	}
// }

// // Add client
// func (s *Server) AddClient(addr *net.UDPAddr) error {
// 	errCh := make(chan error)
// 	s.commands <- &command{
// 		typ:   addClient,
// 		addr:  addr,
// 		errCh: errCh,
// 	}
// 	return <-errCh
// }

// // Handle console input
// func (s *Server) handleInput() {
// 	reader := bufio.NewReader(os.Stdin)
// 	for {
// 		fmt.Print("Server input> ")
// 		line, _ := reader.ReadString('\n')
// 		line = strings.TrimSpace(line)

// 		if strings.HasPrefix(line, "send ") {
// 			parts := strings.SplitN(line, " ", 3)
// 			if len(parts) < 3 {
// 				fmt.Println("Usage: send <clientAddr> <message>")
// 				continue
// 			}
// 			if err := s.SendMessage(parts[1], "[Server] "+parts[2]); err != nil {
// 				fmt.Println("Error:", err)
// 			}
// 		} else if line == "list" {
// 			clients, err := s.ListClients()
// 			if err != nil {
// 				fmt.Println("Error:", err)
// 				continue
// 			}
// 			for _, c := range clients {
// 				fmt.Println("Client:", c)
// 			}
// 		}
// 	}
// }

// // Handle incoming packets
// func (s *Server) handlePackets() {
// 	buffer := make([]byte, 1024)
// 	for {
// 		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
// 		if err != nil {
// 			fmt.Println("Error reading:", err)
// 			continue
// 		}

// 		// Register client
// 		if err := s.AddClient(clientAddr); err != nil {
// 			fmt.Println("Error adding client:", err)
// 		}

// 		// Print message
// 		msg := strings.TrimSpace(string(buffer[:n]))
// 		fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
// 	}
// }

// func main() {
// 	server := NewServer(":10000")
// 	if err := server.Start(); err != nil {
// 		fmt.Println("Server error:", err)
// 	}
// }
