// UDP server
package main

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type commandType int

const (
	addClient commandType = iota
	sendMessage
	listClients
	getClient
	sendFile
	processACK
	completeTransfer
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error
}

type Server struct {
	addr             string
	conn             *net.UDPConn
	commands         chan *command
	BindingMap       map[string]*net.UDPAddr
	PendingMap       map[uint32]*PendingEntry
	ServerLevelQueue chan QueueItem
}

type PendingEntry struct {
	SourceData []byte
	ChunkID    uint32
	Offset     uint64
	Retries    int
	Timer      *time.Timer
	ClientAddr *net.UDPAddr
}

type QueueItem struct {
	Data       []byte
	ClientAddr *net.UDPAddr
}

func NewServer(addr string) *Server {
	return &Server{
		addr:             addr,
		commands:         make(chan *command),
		BindingMap:       make(map[string]*net.UDPAddr),
		PendingMap:       make(map[uint32]*PendingEntry),
		ServerLevelQueue: make(chan QueueItem, 100),
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

	go s.clientManagerWorker()
	go s.handleInput()
	go s.readWorker()

	for w := 0; w < 4; w++ {
		go s.writeWorker()
	}

	return nil
}

func (s *Server) clientManagerWorker() {
	var fileIDCounter uint32 = 0
	for {
		cmd := <-s.commands

		switch cmd.typ {
		case addClient:
			s.BindingMap[cmd.addr.String()] = cmd.addr
			cmd.errCh <- nil

		case sendMessage:
			if clientAddr, ok := s.BindingMap[cmd.targetKey]; ok {
				msg := cmd.message
				p := &Packet{
					Type:     TYPE_MESSAGE,
					Data:     []byte(fmt.Sprintf("[Server] %v", msg)),
					Checksum: computeChecksum([]byte(fmt.Sprintf("[Server] %v", msg))),
				}
				data, err := GeneratePacket(p)
				if err != nil {
					cmd.errCh <- err
					continue
				}
				s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: clientAddr}
				cmd.errCh <- nil
			} else {
				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
			}

		case listClients:
			list := make([]string, 0, len(s.BindingMap))
			for k := range s.BindingMap {
				list = append(list, k)
			}
			cmd.replyCh <- list

		case getClient:
			if clientAddr, ok := s.BindingMap[cmd.targetKey]; ok {
				cmd.replyCh <- clientAddr
			} else {
				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
			}

		case sendFile:
			if clientAddr, ok := s.BindingMap[cmd.targetKey]; ok {
				path := cmd.message.(string)
				fileID := fileIDCounter
				fileIDCounter++
				data, err := os.ReadFile(path)
				if err != nil {
					cmd.errCh <- err
					continue
				}
				fileChecksum := computeChecksum(data)
				fileSize := len(data)
				fileName := filepath.Base(path)
				numChunks := int(math.Ceil(float64(fileSize) / float64(CHUNK_SIZE)))
				for i := 0; i < numChunks; i++ {
					start := i * CHUNK_SIZE
					end := start + CHUNK_SIZE
					if end > fileSize {
						end = fileSize
					}
					chunkData := data[start:end]
					entry := &PendingEntry{
						SourceData: chunkData,
						ChunkID:    uint32(i),
						Offset:     uint64(start),
						Retries:    0,
						ClientAddr: clientAddr,
					}
					s.PendingMap[fileID*100+uint32(i)] = entry
					if i == 0 {
						p := &Packet{
							Type:         TYPE_FILE_META,
							FileID:       fileID,
							ChunkID:      uint32(i),
							Offset:       uint64(start),
							Data:         chunkData,
							Checksum:     computeChecksum(chunkData),
							FileSize:     uint64(fileSize),
							FileName:     fileName,
							TotalChunks:  uint32(numChunks),
							FileChecksum: fileChecksum,
						}
						data, err := GeneratePacket(p)
						if err != nil {
							cmd.errCh <- err
							continue
						}
						s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: clientAddr}
						entry.Timer = time.AfterFunc(ACK_TIMEOUT, func() {
							s.commands <- &command{typ: processACK, targetKey: fmt.Sprintf("%d", fileID), message: ackInfo{chunkID: 0, ok: false}}
						})
					} else {
						p := &Packet{
							Type:     TYPE_FILE_CHUNK,
							FileID:   fileID,
							ChunkID:  uint32(i),
							Offset:   uint64(start),
							Data:     chunkData,
							Checksum: computeChecksum(chunkData),
						}
						data, err := GeneratePacket(p)
						if err != nil {
							cmd.errCh <- err
							continue
						}
						s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: clientAddr}
						entry.Timer = time.AfterFunc(ACK_TIMEOUT, func() {
							s.commands <- &command{typ: processACK, targetKey: fmt.Sprintf("%d", fileID), message: ackInfo{chunkID: uint32(i), ok: false}}
						})
					}
				}
				cmd.errCh <- nil
			} else {
				cmd.errCh <- fmt.Errorf("client not found: %s", cmd.targetKey)
			}

		case processACK:
			fileIDStr := cmd.targetKey
			fileID64, _ := strconv.ParseUint(fileIDStr, 10, 32)
			fileID := uint32(fileID64)
			ai := cmd.message.(ackInfo)
			if entry, ok := s.PendingMap[fileID*100+ai.chunkID]; ok {
				if ai.ok {
					if entry.Timer != nil {
						entry.Timer.Stop()
					}
					delete(s.PendingMap, fileID*100+ai.chunkID)
					if ai.chunkID == 0 && len(s.PendingMap) == 0 {
						for i := 1; i < int(fileIDCounter)*100; i++ {
							if entry, ok := s.PendingMap[uint32(i)]; ok {
								var pType int
								if entry.ChunkID == 0 {
									pType = TYPE_FILE_META
								} else {
									pType = TYPE_FILE_CHUNK
								}
								p := &Packet{
									Type:     pType,
									FileID:   fileID,
									ChunkID:  entry.ChunkID,
									Offset:   entry.Offset,
									Data:     entry.SourceData,
									Checksum: computeChecksum(entry.SourceData),
								}
								data, err := GeneratePacket(p)
								if err != nil {
									continue
								}
								s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: entry.ClientAddr} // استخدام ClientAddr من PendingEntry
								entry.Timer = time.AfterFunc(ACK_TIMEOUT, func() {
									s.commands <- &command{typ: processACK, targetKey: fmt.Sprintf("%d", fileID), message: ackInfo{chunkID: entry.ChunkID, ok: false}}
								})
							}
						}
					}
				} else {
					if entry.Retries < MAX_RETRIES {
						entry.Retries++
						var pType int
						if entry.ChunkID == 0 {
							pType = TYPE_FILE_META
						} else {
							pType = TYPE_FILE_CHUNK
						}
						p := &Packet{
							Type:     pType,
							FileID:   fileID,
							ChunkID:  entry.ChunkID,
							Offset:   entry.Offset,
							Data:     entry.SourceData,
							Checksum: computeChecksum(entry.SourceData),
						}
						data, err := GeneratePacket(p)
						if err != nil {
							continue
						}
						s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: entry.ClientAddr} // استخدام ClientAddr من PendingEntry
						if entry.Timer != nil {
							entry.Timer.Reset(ACK_TIMEOUT)
						} else {
							entry.Timer = time.AfterFunc(ACK_TIMEOUT, func() {
								s.commands <- &command{typ: processACK, targetKey: fmt.Sprintf("%d", fileID), message: ackInfo{chunkID: entry.ChunkID, ok: false}}
							})
						}
					} else {
						if entry.Timer != nil {
							entry.Timer.Stop()
						}
						delete(s.PendingMap, fileID*100+ai.chunkID)
					}
				}
			}

		case completeTransfer:
			fileIDStr := cmd.targetKey
			fileID64, _ := strconv.ParseUint(fileIDStr, 10, 32)
			fileID := uint32(fileID64)
			for i := 0; i < 1000; i++ {
				delete(s.PendingMap, fileID*100+uint32(i))
			}
		}
	}
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
			errCh := make(chan error)
			s.commands <- &command{
				typ:       sendMessage,
				targetKey: parts[1],
				message:   parts[2],
				errCh:     errCh,
			}
			if err := <-errCh; err != nil {
				fmt.Println("Error:", err)
			}
		} else if strings.HasPrefix(line, "sendfile ") {
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 {
				fmt.Println("Usage: sendfile <clientAddr> <filePath>")
				continue
			}
			errCh := make(chan error)
			s.commands <- &command{
				typ:       sendFile,
				targetKey: parts[1],
				message:   parts[2],
				errCh:     errCh,
			}
			if err := <-errCh; err != nil {
				fmt.Println("Error:", err)
			}
		} else if line == "list" {
			replyCh := make(chan any)
			errCh := make(chan error)
			s.commands <- &command{typ: listClients, replyCh: replyCh, errCh: errCh}
			select {
			case data := <-replyCh:
				for _, c := range data.([]string) {
					fmt.Println("Client:", c)
				}
			case err := <-errCh:
				fmt.Println("Error:", err)
			}
		}
	}
}

func (s *Server) readWorker() {
	buffer := make([]byte, 65536)
	for {
		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}
		if err := s.AddClient(clientAddr); err != nil {
			fmt.Println("Error adding client:", err)
		}
		p, err := s.ParsePacket(buffer[:n])
		if err != nil {
			fmt.Println("Error parsing packet:", err)
			continue
		}
		switch p.Type {
		case TYPE_MESSAGE:
			msg := strings.TrimSpace(string(p.Data))
			if msg == "PING" {
				data, err := GeneratePacket(&Packet{Type: TYPE_MESSAGE, Data: []byte("PONG"), Checksum: computeChecksum([]byte("PONG"))})
				if err != nil {
					fmt.Println("Error encoding PONG:", err)
					continue
				}
				s.ServerLevelQueue <- QueueItem{Data: data, ClientAddr: clientAddr}
			}
			fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
		case TYPE_ACK:
			ai := ackInfo{chunkID: p.ChunkID, ok: p.AckStatus}
			s.commands <- &command{typ: processACK, targetKey: fmt.Sprintf("%d", p.FileID), message: ai}
		}
	}
}

func (s *Server) writeWorker() {
	for item := range s.ServerLevelQueue {
		_, err := s.conn.WriteToUDP(item.Data, item.ClientAddr)
		if err != nil {
			fmt.Println("Error writing:", err)
		}
	}
}

func (s *Server) ParsePacket(data []byte) (*Packet, error) {
	p := &Packet{}
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(p); err != nil {
		return nil, err
	}
	return p, nil
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

func (s *Server) SendFile(target string, path string) error {
	errCh := make(chan error)
	s.commands <- &command{
		typ:       sendFile,
		targetKey: target,
		message:   path,
		errCh:     errCh,
	}
	return <-errCh
}

func (s *Server) ListClients() ([]string, error) {
	replyCh := make(chan any)
	errCh := make(chan error)
	s.commands <- &command{typ: listClients, replyCh: replyCh, errCh: errCh}
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
	s.commands <- &command{typ: getClient, targetKey: addr, replyCh: replyCh, errCh: errCh}
	select {
	case data := <-replyCh:
		return data.(*net.UDPAddr), nil
	case err := <-errCh:
		return nil, err
	}
}

func (s *Server) AddClient(addr *net.UDPAddr) error {
	errCh := make(chan error)
	s.commands <- &command{typ: addClient, addr: addr, errCh: errCh}
	return <-errCh
}

func GeneratePacket(p *Packet) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func computeChecksum(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

func main() {
	rand.Seed(time.Now().UnixNano())
	server := NewServer(":10000")
	if err := server.Start(); err != nil {
		fmt.Println("Server error:", err)
	}
	select {}
}

const (
	CHUNK_SIZE  = 60000
	ACK_TIMEOUT = 5 * time.Second
	MAX_RETRIES = 3

	TYPE_MESSAGE    = 0
	TYPE_FILE_META  = 1
	TYPE_FILE_CHUNK = 2
	TYPE_ACK        = 3
)

type Packet struct {
	Type         int
	FileID       uint32
	ChunkID      uint32
	Offset       uint64
	Data         []byte
	Checksum     uint32
	FileSize     uint64
	FileName     string
	TotalChunks  uint32
	FileChecksum uint32
	AckStatus    bool
}

type ackInfo struct {
	chunkID uint32
	ok      bool
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
// 		if msg == "PING" {
// 			s.SendMessage(clientAddr.String(), "PONG")
// 		}
// 		fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
// 	}
// }

// func main() {
// 	server := NewServer(":10000")
// 	if err := server.Start(); err != nil {
// 		fmt.Println("Server error:", err)
// 	}
// }
