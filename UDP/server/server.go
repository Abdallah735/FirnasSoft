// UDP server
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

type commandType int

const (
	addClient commandType = iota
	sendMessage
	listClients
	getClient
)

type command struct {
	typ       commandType
	addr      *net.UDPAddr
	targetKey string
	message   any
	replyCh   chan any
	errCh     chan error
}

// ----------------- Server Struct -----------------
type Server struct {
	addr     string
	conn     *net.UDPConn
	commands chan *command
}

// Create new server
func NewServer(addr string) *Server {
	return &Server{
		addr:     addr,
		commands: make(chan *command),
	}
}

// Start server
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

// Manager goroutine
func (s *Server) clientManagerWorker() {
	clients := make(map[string]*net.UDPAddr)

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
		}
	}
}

// Send message
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

// Handle console input
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

// Handle incoming packets
func (s *Server) handlePackets() {
	buffer := make([]byte, 1024)
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

		// Print message
		msg := strings.TrimSpace(string(buffer[:n]))
		fmt.Printf("Message from %s: %s\n", clientAddr.String(), msg)
	}
}

func main() {
	server := NewServer(":10000")
	if err := server.Start(); err != nil {
		fmt.Println("Server error:", err)
	}
}

//----------------------------------------
// package main

// import (
// 	"bufio"
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
// )

// type command struct {
// 	typ       commandType
// 	addr      *net.UDPAddr
// 	targetKey string
// 	message   string
// 	replyCh   chan []string
// 	//error
// }

// // ----------------- Server Struct -----------------
// type Server struct {
// 	addr     string
// 	conn     *net.UDPConn
// 	commands chan command
// }

// // Create new server
// func NewServer(addr string) *Server {
// 	return &Server{
// 		addr:     addr,
// 		commands: make(chan command),
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
// 	go s.clientManager()

// 	// Start console input
// 	go s.handleInput()

// 	// Start handling packets
// 	s.handlePackets()

// 	return nil
// }

// // Manager goroutine
// func (s *Server) clientManager() {  //name worker
// 	clients := make(map[string]*net.UDPAddr)

// 	for cmd := range s.commands {  //use infinity loop
// 		switch cmd.typ {
// 		case addClient:
// 			if _, exists := clients[cmd.addr.String()]; !exists {
// 				fmt.Println("New client registered:", cmd.addr)
// 			}
// 			clients[cmd.addr.String()] = cmd.addr

// 		case sendMessage:
// 			if client, ok := clients[cmd.targetKey]; ok {
// 				s.conn.WriteToUDP([]byte(cmd.message), client)
// 			} else {
// 				fmt.Println("Client not found:", cmd.targetKey)
// 			}

// 		case listClients:
// 			list := make([]string, 0, len(clients))
// 			for k := range clients {
// 				list = append(list, k)
// 			}
// 			cmd.replyCh <- list
// 		}
// 	}
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
// 			s.commands <- command{
// 				typ:       sendMessage,
// 				targetKey: parts[1],
// 				message:   "[Server] " + parts[2],
// 			}
// 		} else if line == "list" {
// 			replyCh := make(chan []string)
// 			s.commands <- command{typ: listClients, replyCh: replyCh}
// 			clients := <-replyCh
// 			for _, c := range clients {
// 				fmt.Println("Client:", c)
// 			}
// 		}
// 	}
// }

// // Handle client packets
// func (s *Server) handlePackets() {
// 	buffer := make([]byte, 1024)
// 	for {
// 		n, clientAddr, err := s.conn.ReadFromUDP(buffer)
// 		if err != nil {
// 			fmt.Println("Error reading:", err)
// 			continue
// 		}

// 		// Register/update client
// 		s.commands <- command{typ: addClient, addr: clientAddr}

// 		data := make([]byte, n)
// 		copy(data, buffer[:n])

// 		go s.handleMessage(data, clientAddr)
// 	}
// }

// // Handle single client message
// func (s *Server) handleMessage(pkt []byte, addr *net.UDPAddr) {
// 	msg := string(pkt)
// 	fmt.Printf("Received from %v: %s\n", addr, msg)

// 	if msg == "PING" {
// 		s.conn.WriteToUDP([]byte("PONG"), addr)
// 		return
// 	}

// 	reply := fmt.Sprintf("Echo: %s", msg)
// 	s.conn.WriteToUDP([]byte(reply), addr)
// }

// // ----------------- main -----------------
// func main() {
// 	server := NewServer("173.208.144.109:10000")
// 	if err := server.Start(); err != nil {
// 		fmt.Println("Server error:", err)
// 	}
// }

//------------------------------------------------

// package main

// import (
// 	"bufio"
// 	"fmt"
// 	"net"
// 	"os"
// 	"strings"
// 	"sync"
// 	"time"
// )

// func main() {
// 	addr, err := net.ResolveUDPAddr("udp", "173.208.144.109:10000")
// 	if err != nil {
// 		fmt.Println("Error resolving address:", err)
// 		return
// 	}

// 	conn, err := net.ListenUDP("udp", addr)
// 	if err != nil {
// 		fmt.Println("Error listening:", err)
// 		return
// 	}
// 	defer conn.Close()

// 	fmt.Println("UDP server listening on port 10000")

// 	clients := make(map[string]*net.UDPAddr)
// 	var mu sync.Mutex

// 	go func() {
// 		reader := bufio.NewReader(os.Stdin)
// 		for {
// 			fmt.Print("Server input> ")
// 			line, _ := reader.ReadString('\n')
// 			line = strings.TrimSpace(line)

// 			if strings.HasPrefix(line, "send ") {
// 				parts := strings.SplitN(line, " ", 3)
// 				if len(parts) < 3 {
// 					fmt.Println("Usage: send <clientAddr> <message>")
// 					continue
// 				}
// 				targetAddr := parts[1]
// 				message := parts[2]

// 				mu.Lock()
// 				client, ok := clients[targetAddr]
// 				mu.Unlock()
// 				if !ok {
// 					fmt.Println("Client not found:", targetAddr)
// 					continue
// 				}

// 				_, err := conn.WriteToUDP([]byte("[Server] "+message), client)
// 				if err != nil {
// 					fmt.Println("Error sending to", targetAddr, ":", err)
// 				} else {
// 					fmt.Println("Message sent to", targetAddr)
// 				}
// 			} else if line == "list" {
// 				mu.Lock()
// 				for c := range clients {
// 					fmt.Println("Client:", c)
// 				}
// 				mu.Unlock()
// 			}
// 		}
// 	}()

// 	go func() {
// 		ticker := time.NewTicker(10 * time.Minute)
// 		defer ticker.Stop()
// 		for range ticker.C {
// 			mu.Lock()
// 			for _, client := range clients {
// 				conn.WriteToUDP([]byte("I remember you"), client)
// 			}
// 			mu.Unlock()
// 		}
// 	}()

// 	buffer := make([]byte, 1024)
// 	for {
// 		n, clientAddr, err := conn.ReadFromUDP(buffer)
// 		if err != nil {
// 			fmt.Println("Error reading:", err)
// 			continue
// 		}

// 		mu.Lock()
// 		clients[clientAddr.String()] = clientAddr
// 		mu.Unlock()

// 		data := make([]byte, n)
// 		copy(data, buffer[:n])

// 		go func(pkt []byte, addr *net.UDPAddr) {
// 			msg := string(pkt)
// 			fmt.Printf("Received from %v: %s\n", addr, msg)

// 			if msg == "PING" {
// 				conn.WriteToUDP([]byte("PONG"), addr)
// 				return
// 			}

// 			reply := fmt.Sprintf("Echo: %s", msg)
// 			_, err := conn.WriteToUDP([]byte(reply), addr)
// 			if err != nil {
// 				fmt.Println("Error writing to", addr, ":", err)
// 			}
// 		}(data, clientAddr)
// 	}
// }
