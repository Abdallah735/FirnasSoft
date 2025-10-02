// UDP server
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
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
	recvPacket // NEW: packet delivered to manager for processing
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error
}

// Packet header exchanged in UDP packets (header JSON then '\n' then payload)
type PacketHeader struct {
	Type        string `json:"type"`                   // "INIT", "CHUNK", "ACK_INIT", "ACK_CHUNK", "ACK_COMPLETE"
	Filename    string `json:"filename,omitempty"`     // for INIT
	Filesize    int64  `json:"filesize,omitempty"`     // for INIT
	ChunkSeq    int    `json:"seq,omitempty"`          // for CHUNK / ACK_CHUNK
	ChunkSize   int    `json:"chunk_size,omitempty"`   // used in INIT (suggested)
	FileID      string `json:"file_id,omitempty"`      // assigned by server
	TotalChunks int    `json:"total_chunks,omitempty"` // used in INIT
	Reason      string `json:"reason,omitempty"`       // optional
}

type ReceivedPacket struct {
	From   *net.UDPAddr
	Header PacketHeader
	Data   []byte
}

type Transfer struct {
	FileID      string
	Filename    string
	Filesize    int64
	ChunkSize   int
	TotalChunks int
	Received    map[int]bool
	FilePath    string
	File        *os.File
	Owner       *net.UDPAddr
	CreatedAt   time.Time
}

type Server struct {
	addr     string
	conn     *net.UDPConn
	commands chan *command
}

// NewServer creates server
func NewServer(addr string) *Server {
	// seed rand for file id generation
	rand.Seed(time.Now().UnixNano())
	return &Server{
		addr:     addr,
		commands: make(chan *command),
	}
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

	// Start manager
	go s.clientManagerWorker()

	// Start console input
	go s.handleInput()

	// Start handling packets
	s.handlePackets()

	return nil
}

func (s *Server) clientManagerWorker() {
	clients := make(map[string]*net.UDPAddr)
	transfers := make(map[string]*Transfer) // fileID -> Transfer

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

		case recvPacket:
			rp, ok := cmd.message.(*ReceivedPacket)
			if !ok {
				if cmd.errCh != nil {
					cmd.errCh <- fmt.Errorf("invalid recvPacket message")
				}
				continue
			}

			// handle INIT and CHUNK
			switch rp.Header.Type {
			case "INIT":
				// create unique fileID
				fileID := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
				uploadsDir := "uploads"
				_ = os.MkdirAll(uploadsDir, 0o755)
				safeName := filepath.Base(rp.Header.Filename)
				filePath := filepath.Join(uploadsDir, fmt.Sprintf("%s_%s.tmp", fileID, safeName))

				// create and truncate file to full size
				f, err := os.Create(filePath)
				if err != nil {
					fmt.Println("Error creating file:", err)
					// reply NAK?
					resp := PacketHeader{Type: "ACK_INIT", FileID: "", Reason: "server_file_create_error"}
					s.sendHeaderTo(resp, rp.From)
					continue
				}
				if err := f.Truncate(rp.Header.Filesize); err != nil {
					fmt.Println("Error truncating file:", err)
					f.Close()
					resp := PacketHeader{Type: "ACK_INIT", FileID: "", Reason: "server_truncate_error"}
					s.sendHeaderTo(resp, rp.From)
					continue
				}

				tr := &Transfer{
					FileID:      fileID,
					Filename:    safeName,
					Filesize:    rp.Header.Filesize,
					ChunkSize:   rp.Header.ChunkSize,
					TotalChunks: rp.Header.TotalChunks,
					Received:    make(map[int]bool),
					FilePath:    filePath,
					File:        f,
					Owner:       rp.From,
					CreatedAt:   time.Now(),
				}
				transfers[fileID] = tr

				// send ACK_INIT with file_id
				resp := PacketHeader{Type: "ACK_INIT", FileID: fileID}
				s.sendHeaderTo(resp, rp.From)
				fmt.Printf("INIT received from %s -> fileID=%s filename=%s size=%d chunks=%d\n", rp.From.String(), fileID, safeName, rp.Header.Filesize, rp.Header.TotalChunks)

			case "CHUNK":
				fileID := rp.Header.FileID
				tr, ok := transfers[fileID]
				if !ok {
					// unknown file_id -> ask re-init
					resp := PacketHeader{Type: "ACK_CHUNK", FileID: fileID, ChunkSeq: rp.Header.ChunkSeq, Reason: "unknown_file_id"}
					s.sendHeaderTo(resp, rp.From)
					continue
				}

				seq := rp.Header.ChunkSeq
				offset := int64(seq) * int64(tr.ChunkSize)

				// write payload at offset
				if len(rp.Data) > 0 {
					n, err := tr.File.WriteAt(rp.Data, offset)
					if err != nil || n != len(rp.Data) {
						fmt.Printf("Error writing chunk %d for %s: %v (wrote %d of %d)\n", seq, fileID, err, n, len(rp.Data))
						resp := PacketHeader{Type: "ACK_CHUNK", FileID: fileID, ChunkSeq: seq, Reason: "write_error"}
						s.sendHeaderTo(resp, rp.From)
						continue
					}
				}

				// mark received
				tr.Received[seq] = true
				// ack the chunk
				resp := PacketHeader{Type: "ACK_CHUNK", FileID: fileID, ChunkSeq: seq}
				s.sendHeaderTo(resp, rp.From)

				// check completion
				if len(tr.Received) >= tr.TotalChunks {
					// close and finalize (rename .tmp -> final)
					tr.File.Close()
					finalPath := filepath.Join("uploads", fmt.Sprintf("%s_%s", fileID, tr.Filename))
					if err := os.Rename(tr.FilePath, finalPath); err != nil {
						fmt.Println("Error renaming final file:", err)
					} else {
						fmt.Printf("File complete: %s from %s\n", finalPath, rp.From.String())
					}
					// send completion ack
					comp := PacketHeader{Type: "ACK_COMPLETE", FileID: fileID}
					s.sendHeaderTo(comp, rp.From)
					delete(transfers, fileID)
				}

			default:
				// ignore or log
				fmt.Println("Unknown packet type:", rp.Header.Type)
			}
		}
	}
}

// helper to send header as JSON only
func (s *Server) sendHeaderTo(h PacketHeader, to *net.UDPAddr) {
	b, _ := json.Marshal(h)
	// send JSON only (no payload)
	_, err := s.conn.WriteToUDP(b, to)
	if err != nil {
		fmt.Println("Error sending header to", to.String(), ":", err)
	}
}

// Send message (unchanged interface)
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

// List clients
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

// Get client
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

// Add client
func (s *Server) AddClient(addr *net.UDPAddr) error {
	errCh := make(chan error)
	s.commands <- &command{
		typ:   addClient,
		addr:  addr,
		errCh: errCh,
	}
	return <-errCh
}

// Handle console input (unchanged except small message)
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

// Handle incoming packets: parse header JSON (up to first '\n') then payload
func (s *Server) handlePackets() {
	buffer := make([]byte, 65536) // large enough for UDP packet
	for {
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}

		// Register client (async via commands)
		if err := s.AddClient(clientAddr); err != nil {
			fmt.Println("Error adding client:", err)
		}

		data := make([]byte, n)
		copy(data, buffer[:n])

		// split header/payload at first '\n'
		headerEnd := -1
		for i := 0; i < len(data); i++ {
			if data[i] == '\n' {
				headerEnd = i
				break
			}
		}

		var hdr PacketHeader
		var payload []byte

		if headerEnd == -1 {
			// assume whole packet is header JSON (ACKs, etc)
			if err := json.Unmarshal(data, &hdr); err != nil {
				fmt.Println("Invalid header-only packet from", clientAddr.String(), "err:", err)
				continue
			}
		} else {
			if err := json.Unmarshal(data[:headerEnd], &hdr); err != nil {
				fmt.Println("Invalid header from", clientAddr.String(), "err:", err)
				continue
			}
			payload = data[headerEnd+1:]
		}

		rp := &ReceivedPacket{
			From:   clientAddr,
			Header: hdr,
			Data:   payload,
		}
		// hand over to manager for handling (no mutex anywhere)
		s.commands <- &command{
			typ:     recvPacket,
			message: rp,
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
