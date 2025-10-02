// UDP server
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type commandType int

const (
	addClient commandType = iota
	sendMessage
	listClients
	getClient
	recvPacket
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error
}

// file receiver state (owned by manager goroutine)
type fileRecv struct {
	ID         string
	OwnerAddr  *net.UDPAddr
	FileName   string
	TotalSize  int64
	ChunkSize  int
	TotalParts int
	Received   map[int]bool
	TempPath   string
	CreatedAt  time.Time
}

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

	// Start console input
	go s.handleInput()

	// Start handling packets
	s.handlePackets()

	return nil
}

func genFileID() (string, error) {
	b := make([]byte, 12)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) clientManagerWorker() {
	clients := make(map[string]*net.UDPAddr)
	files := make(map[string]*fileRecv) // fileID -> state

	for {
		cmd := <-s.commands

		switch cmd.typ {
		case addClient:
			clients[cmd.addr.String()] = cmd.addr
			if cmd.errCh != nil {
				cmd.errCh <- nil
			}

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

		case recvPacket:
			// message expected: []byte
			raw, ok := cmd.message.([]byte)
			if !ok {
				if cmd.errCh != nil {
					cmd.errCh <- fmt.Errorf("invalid packet payload type")
				}
				continue
			}
			clientAddr := cmd.addr

			// split header\npayload
			idx := -1
			for i := 0; i < len(raw); i++ {
				if raw[i] == '\n' {
					idx = i
					break
				}
			}
			if idx == -1 {
				// invalid packet
				fmt.Println("invalid packet (no header newline) from", clientAddr.String())
				continue
			}

			var hdr struct {
				Type      string `json:"Type"`
				FileName  string `json:"FileName,omitempty"`
				FileSize  int64  `json:"FileSize,omitempty"`
				Seq       int    `json:"Seq,omitempty"`
				ChunkSize int    `json:"ChunkSize,omitempty"`
				FileID    string `json:"FileID,omitempty"`
			}

			if err := json.Unmarshal(raw[:idx], &hdr); err != nil {
				fmt.Println("bad header JSON:", err)
				continue
			}
			payload := raw[idx+1:]

			switch strings.ToUpper(hdr.Type) {
			case "FIRST":
				// create file entry, server generates fileID
				fileID, err := genFileID()
				if err != nil {
					fmt.Println("unable to generate fileID:", err)
					continue
				}
				tmpDir := "uploaded_files"
				os.MkdirAll(tmpDir, 0755)
				tempPath := filepath.Join(tmpDir, fileID+".tmp")
				f, err := os.Create(tempPath)
				if err != nil {
					fmt.Println("unable to create temp file:", err)
					continue
				}
				// pre-allocate
				if hdr.FileSize > 0 {
					if err := f.Truncate(hdr.FileSize); err != nil {
						fmt.Println("truncate error:", err)
						f.Close()
						continue
					}
				}
				f.Close()

				totalParts := 1
				if hdr.ChunkSize > 0 {
					totalParts = int((hdr.FileSize + int64(hdr.ChunkSize) - 1) / int64(hdr.ChunkSize))
				}

				fr := &fileRecv{
					ID:         fileID,
					OwnerAddr:  clientAddr,
					FileName:   hdr.FileName,
					TotalSize:  hdr.FileSize,
					ChunkSize:  hdr.ChunkSize,
					TotalParts: totalParts,
					Received:   make(map[int]bool),
					TempPath:   tempPath,
					CreatedAt:  time.Now(),
				}
				files[fileID] = fr

				// write the first chunk at Seq position
				offset := int64(hdr.Seq) * int64(hdr.ChunkSize)
				if f2, err := os.OpenFile(fr.TempPath, os.O_WRONLY, 0644); err == nil {
					_, werr := f2.WriteAt(payload, offset)
					if werr != nil {
						fmt.Println("writeAt error:", werr)
					} else {
						fr.Received[hdr.Seq] = true
					}
					f2.Close()
				} else {
					fmt.Println("open temp for write error:", err)
				}

				// respond with FileID ack so client will use it for remaining chunks
				resp, _ := json.Marshal(map[string]any{
					"Type":   "FILEID",
					"FileID": fileID,
					"Seq":    hdr.Seq,
				})
				_, _ = s.conn.WriteToUDP(resp, clientAddr)
				fmt.Printf("Started receiving file %s from %s as id=%s totalParts=%d\n", fr.FileName, clientAddr.String(), fileID, fr.TotalParts)

				// check completion
				if len(fr.Received) == fr.TotalParts {
					final := filepath.Join(tmpDir, fr.FileName)
					os.Rename(fr.TempPath, final)
					comp, _ := json.Marshal(map[string]any{
						"Type":   "COMPLETE",
						"FileID": fileID,
						"Path":   final,
					})
					_, _ = s.conn.WriteToUDP(comp, clientAddr)
					delete(files, fileID)
					fmt.Printf("File complete: %s (from %s)\n", final, clientAddr.String())
				}

			case "CHUNK":
				// expect FileID present
				fr, ok := files[hdr.FileID]
				if !ok {
					// unknown file; tell client to retry or error
					errMsg, _ := json.Marshal(map[string]any{
						"Type":   "ERROR",
						"Error":  "unknown_file",
						"FileID": hdr.FileID,
					})
					_, _ = s.conn.WriteToUDP(errMsg, clientAddr)
					continue
				}
				offset := int64(hdr.Seq) * int64(fr.ChunkSize)
				if f2, err := os.OpenFile(fr.TempPath, os.O_WRONLY, 0644); err == nil {
					_, werr := f2.WriteAt(payload, offset)
					if werr != nil {
						fmt.Println("writeAt error:", werr)
					} else {
						fr.Received[hdr.Seq] = true
					}
					f2.Close()
				} else {
					fmt.Println("open temp for write error:", err)
					continue
				}

				// ack this chunk
				ack, _ := json.Marshal(map[string]any{
					"Type":   "ACK",
					"FileID": fr.ID,
					"Seq":    hdr.Seq,
				})
				_, _ = s.conn.WriteToUDP(ack, clientAddr)

				// if completed -> rename
				if len(fr.Received) == fr.TotalParts {
					final := filepath.Join("uploaded_files", fr.FileName)
					os.Rename(fr.TempPath, final)
					comp, _ := json.Marshal(map[string]any{
						"Type":   "COMPLETE",
						"FileID": fr.ID,
						"Path":   final,
					})
					_, _ = s.conn.WriteToUDP(comp, clientAddr)
					delete(files, fr.ID)
					fmt.Printf("File complete: %s (from %s)\n", final, clientAddr.String())
				}

			default:
				// treat as plain text message
				fmt.Printf("Message from %s: %s\n", clientAddr.String(), strings.TrimSpace(string(payload)))
			}

		}
	}
}

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

func (s *Server) handlePackets() {
	buffer := make([]byte, 65535)
	for {
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}

		// register client (non-blocking style using manager)
		go func(addr *net.UDPAddr) {
			_ = s.AddClient(addr)
		}(clientAddr)

		// copy bytes because buffer is reused
		data := make([]byte, n)
		copy(data, buffer[:n])

		// send to manager for processing
		s.commands <- &command{
			typ:  recvPacket,
			addr: clientAddr,
			// message carries raw packet bytes
			message: data,
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
