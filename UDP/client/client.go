// UDP client
package main

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type UDPClient struct {
	serverAddr   *net.UDPAddr
	conn         *net.UDPConn
	receiveChan  chan *Packet
	fileReceives map[uint32]*FileReceive
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

	client := &UDPClient{
		serverAddr:   addr,
		conn:         conn,
		receiveChan:  make(chan *Packet),
		fileReceives: make(map[uint32]*FileReceive),
	}

	return client, nil
}

func (c *UDPClient) Start() {
	fmt.Println("UDP client started. Connected to", c.serverAddr.String())
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")

	// Start receiver
	go func() {
		buffer := make([]byte, 65536)
		for {
			n, _, err := c.conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading from server:", err)
				continue
			}
			p, err := decodePacket(buffer[:n])
			if err != nil {
				fmt.Println("Error decoding packet:", err)
				continue
			}
			c.receiveChan <- p
		}
	}()

	// Start receive worker
	go c.receiveWorker()

	// Start keep-alive
	go func() {
		ticker := time.NewTicker(28 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			data, err := GeneratePacket(&Packet{Type: TYPE_MESSAGE, Data: []byte("PING"), Checksum: computeChecksum([]byte("PING"))})
			if err != nil {
				fmt.Println("Error encoding keep-alive:", err)
				continue
			}
			_, err = c.conn.Write(data)
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

		data, err := GeneratePacket(&Packet{Type: TYPE_MESSAGE, Data: []byte(text), Checksum: computeChecksum([]byte(text))})
		if err != nil {
			fmt.Println("Error encoding message:", err)
			continue
		}
		_, err = c.conn.Write(data)
		if err != nil {
			fmt.Println("Error writing to server:", err)
		}
	}
}

func (c *UDPClient) receiveWorker() {
	for p := range c.receiveChan {
		switch p.Type {
		case TYPE_MESSAGE:
			fmt.Println("\n[Server]:", string(p.Data))
			fmt.Print(">> ")
		case TYPE_FILE_META:
			if p.Checksum != computeChecksum(p.Data) {
				c.sendAck(p.FileID, p.ChunkID, false)
				continue
			}
			fr := &FileReceive{}
			file, err := os.Create(p.FileName)
			if err != nil {
				fmt.Println("Error creating file:", err)
				c.sendAck(p.FileID, p.ChunkID, false)
				continue
			}
			fr.file = file
			fr.fileSize = p.FileSize
			fr.fileName = p.FileName
			fr.totalChunks = p.TotalChunks
			fr.expectedFileChecksum = p.FileChecksum
			fr.received = make(map[uint32]bool)
			_, err = file.WriteAt(p.Data, int64(p.Offset))
			if err != nil {
				fmt.Println("Error writing meta chunk:", err)
				c.sendAck(p.FileID, p.ChunkID, false)
				file.Close()
				continue
			}
			fr.received[p.ChunkID] = true
			c.fileReceives[p.FileID] = fr
			c.sendAck(p.FileID, p.ChunkID, true)
			if uint32(len(fr.received)) == fr.totalChunks {
				verified := c.verifyFileChecksum(fr)
				fr.file.Close()
				if verified {
					fmt.Println("File received and verified successfully:", fr.fileName)
				} else {
					fmt.Println("File received but checksum verification failed:", fr.fileName)
				}
				delete(c.fileReceives, p.FileID)
			}
		case TYPE_FILE_CHUNK:
			fr, ok := c.fileReceives[p.FileID]
			if !ok {
				c.sendAck(p.FileID, p.ChunkID, false)
				continue
			}
			if p.Checksum != computeChecksum(p.Data) {
				c.sendAck(p.FileID, p.ChunkID, false)
				continue
			}
			_, err := fr.file.WriteAt(p.Data, int64(p.Offset))
			if err != nil {
				fmt.Println("Error writing chunk:", err)
				c.sendAck(p.FileID, p.ChunkID, false)
				continue
			}
			fr.received[p.ChunkID] = true
			c.sendAck(p.FileID, p.ChunkID, true)
			if uint32(len(fr.received)) == fr.totalChunks {
				verified := c.verifyFileChecksum(fr)
				fr.file.Close()
				if verified {
					fmt.Println("File received and verified successfully:", fr.fileName)
				} else {
					fmt.Println("File received but checksum verification failed:", fr.fileName)
				}
				delete(c.fileReceives, p.FileID)
			}
		}
	}
}

func (c *UDPClient) verifyFileChecksum(fr *FileReceive) bool {
	fr.file.Sync()
	_, err := fr.file.Seek(0, io.SeekStart)
	if err != nil {
		fmt.Println("Error seeking file for verification:", err)
		return false
	}
	data, err := io.ReadAll(fr.file)
	if err != nil {
		fmt.Println("Error reading file for verification:", err)
		return false
	}
	computed := computeChecksum(data)
	return computed == fr.expectedFileChecksum
}

func (c *UDPClient) sendAck(fileID uint32, chunkID uint32, ok bool) {
	data, err := GeneratePacket(&Packet{
		Type:      TYPE_ACK,
		FileID:    fileID,
		ChunkID:   chunkID,
		AckStatus: ok,
	})
	if err != nil {
		fmt.Println("Error encoding ACK/NACK:", err)
		return
	}
	_, err = c.conn.Write(data)
	if err != nil {
		fmt.Println("Error sending ACK/NACK:", err)
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

// Constants (shared with server)
const (
	CHUNK_SIZE = 60000

	TYPE_MESSAGE    = 0
	TYPE_FILE_META  = 1
	TYPE_FILE_CHUNK = 2
	TYPE_ACK        = 3
)

// Packet struct (shared)
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

// File receive
type FileReceive struct {
	file                 *os.File
	fileSize             uint64
	fileName             string
	totalChunks          uint32
	expectedFileChecksum uint32
	received             map[uint32]bool
}

func GeneratePacket(p *Packet) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	// Enc/Dec Information (مستقبلي: هنا ممكن نضيف encryption)
	return buf.Bytes(), nil
}

func decodePacket(data []byte) (*Packet, error) {
	p := &Packet{}
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(p); err != nil {
		return nil, err
	}
	return p, nil
}

func computeChecksum(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
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
