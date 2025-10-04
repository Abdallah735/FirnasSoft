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
	"strings"
	"time"
)

type commandType int

const (
	addClient commandType = iota
	sendMessage
	listClients
	getClient
	processChunk
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error

	// for chunk handling
	rawData []byte
	n       int
}

// Server structure
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

// bookkeeping per file (accessed only inside manager goroutine)
type FileReceive struct {
	fileID       string
	ownerAddr    string
	filePath     string
	file         *os.File
	totalSize    int64
	chunkSize    int
	received     map[int]bool
	lastActivity time.Time
}

func (s *Server) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		return fmt.Errorf("Error resolving address: %v", err)
	}

	s.conn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("Error listening: %v", err)
	}

	fmt.Println("UDP server listening on", s.addr)

	// start manager
	go s.clientManagerWorker()

	// console input (send/list)
	go s.handleInput()

	// handle packets
	go s.handlePackets()

	return nil
}

func (s *Server) clientManagerWorker() {
	clients := make(map[string]*net.UDPAddr)
	files := make(map[string]*FileReceive)

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

		case processChunk:
			// parse header JSON until newline, then payload
			buf := cmd.rawData[:cmd.n]
			idx := -1
			for i := 0; i < len(buf); i++ {
				if buf[i] == '\n' {
					idx = i
					break
				}
			}
			if idx == -1 {
				fmt.Println("Invalid chunk packet: no header newline")
				continue
			}
			headerJSON := string(buf[:idx])
			payload := buf[idx+1 : cmd.n]

			var hdr struct {
				First     bool   `json:"first"`
				Seq       int    `json:"seq"`
				ChunkSize int    `json:"chunk_size"`
				TotalSize int64  `json:"total_size,omitempty"`
				FileName  string `json:"file_name,omitempty"`
				FileID    string `json:"file_id,omitempty"`
			}
			if err := json.Unmarshal([]byte(headerJSON), &hdr); err != nil {
				fmt.Println("Header unmarshal error:", err)
				continue
			}

			sender := cmd.addr.String()

			if hdr.First {
				// create new file receive
				r := make([]byte, 12)
				if _, err := rand.Read(r); err != nil {
					fmt.Println("rand error:", err)
					continue
				}
				fileID := hex.EncodeToString(r)
				fileName := hdr.FileName
				if fileName == "" {
					fileName = "upload"
				}
				filePath := "./received_" + fileID + "_" + sanitize(fileName)

				f, err := os.Create(filePath)
				if err != nil {
					fmt.Println("Error creating file:", err)
					continue
				}
				if hdr.TotalSize > 0 {
					if err := f.Truncate(hdr.TotalSize); err != nil {
						fmt.Println("Warning: truncate failed:", err)
					}
				}

				fr := &FileReceive{
					fileID:       fileID,
					ownerAddr:    sender,
					filePath:     filePath,
					file:         f,
					totalSize:    hdr.TotalSize,
					chunkSize:    hdr.ChunkSize,
					received:     make(map[int]bool),
					lastActivity: time.Now(),
				}
				files[fileID] = fr

				// write payload at offset
				offset := int64(hdr.Seq) * int64(hdr.ChunkSize)
				if _, err := f.WriteAt(payload, offset); err != nil {
					fmt.Println("WriteAt error:", err)
				} else {
					fr.received[hdr.Seq] = true
					fr.lastActivity = time.Now()
					fmt.Printf("Received first chunk for file %s from %s seq=%d size=%d\n", fileID, sender, hdr.Seq, len(payload))
				}

				// send ACK with file_id
				ack := map[string]any{
					"type":    "ack",
					"file_id": fileID,
					"seq":     hdr.Seq,
					"status":  "ok",
				}
				ackB, _ := json.Marshal(ack)
				if _, err := s.conn.WriteToUDP(ackB, cmd.addr); err != nil {
					fmt.Println("Error sending ack:", err)
				}

			} else {
				// normal chunk
				fileID := hdr.FileID
				if fileID == "" {
					fmt.Println("Chunk without file_id from", sender)
					nak := map[string]any{"type": "nak", "reason": "no_file_id", "seq": hdr.Seq}
					nb, _ := json.Marshal(nak)
					_, _ = s.conn.WriteToUDP(nb, cmd.addr)
					continue
				}
				fr, ok := files[fileID]
				if !ok {
					// unknown file id
					fmt.Println("Unknown file_id", fileID, "from", sender)
					nak := map[string]any{"type": "nak", "reason": "unknown_file_id", "seq": hdr.Seq}
					nb, _ := json.Marshal(nak)
					_, _ = s.conn.WriteToUDP(nb, cmd.addr)
					continue
				}
				// check owner matches
				if fr.ownerAddr != sender {
					fmt.Println("Owner mismatch for file", fileID, "from", sender, "expected", fr.ownerAddr)
					nak := map[string]any{"type": "nak", "reason": "owner_mismatch", "seq": hdr.Seq}
					nb, _ := json.Marshal(nak)
					_, _ = s.conn.WriteToUDP(nb, cmd.addr)
					continue
				}

				// if already received, ack again
				if fr.received[hdr.Seq] {
					ack := map[string]any{"type": "ack", "file_id": fileID, "seq": hdr.Seq, "status": "ok"}
					ackB, _ := json.Marshal(ack)
					_, _ = s.conn.WriteToUDP(ackB, cmd.addr)
					continue
				}

				offset := int64(hdr.Seq) * int64(fr.chunkSize)
				if _, err := fr.file.WriteAt(payload, offset); err != nil {
					fmt.Println("WriteAt error:", err)
				} else {
					fr.received[hdr.Seq] = true
					fr.lastActivity = time.Now()
					fmt.Printf("Wrote chunk for file %s seq=%d size=%d\n", fileID, hdr.Seq, len(payload))
				}

				ack := map[string]any{
					"type":    "ack",
					"file_id": fileID,
					"seq":     hdr.Seq,
					"status":  "ok",
				}
				ackB, _ := json.Marshal(ack)
				if _, err := s.conn.WriteToUDP(ackB, cmd.addr); err != nil {
					fmt.Println("Error sending ack:", err)
				}

				// check for completion (if totalSize known)
				if fr.totalSize > 0 && fr.chunkSize > 0 {
					expected := int((fr.totalSize + int64(fr.chunkSize) - 1) / int64(fr.chunkSize))
					if len(fr.received) >= expected {
						// close file and report complete
						_ = fr.file.Close()
						fmt.Printf("File %s complete -> %s\n", fileID, fr.filePath)
						// optionally remove from map to free memory
						delete(files, fileID)
					}
				}
			}
		}
	}
}

func sanitize(name string) string {
	return strings.ReplaceAll(name, "/", "_")
}

// SendMessage unchanged API
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

func (s *Server) handlePackets() {
	buffer := make([]byte, 70000)
	for {
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}
		// register
		if err := s.AddClient(clientAddr); err != nil {
			fmt.Println("Error adding client:", err)
		}

		if n > 0 && buffer[0] == '{' {
			// chunk packet
			raw := make([]byte, n)
			copy(raw, buffer[:n])
			s.commands <- &command{
				typ:     processChunk,
				addr:    clientAddr,
				rawData: raw,
				n:       n,
			}
			continue
		}

		msg := strings.TrimSpace(string(buffer[:n]))
		fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
	}
}

func (s *Server) handleInput() {
	reader := bufioNewReader(os.Stdin)
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

// small helper (we avoid importing bufio at top to keep imports grouped — add it)
func bufioNewReader(r *os.File) *bufio.Reader {
	return bufio.NewReader(r)
}

func main() {
	server := NewServer(":10000")
	if err := server.Start(); err != nil {
		fmt.Println("Server error:", err)
	}
	select {}
}

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
