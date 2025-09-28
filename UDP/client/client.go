// UDP client
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// ----------------- Client Struct -----------------
type Client struct {
	serverAddr string
	conn       *net.UDPConn
}

// Create new client
func NewClient(serverAddr string) *Client {
	return &Client{serverAddr: serverAddr}
}

// Start client
func (c *Client) Start() error {
	addr, err := net.ResolveUDPAddr("udp", c.serverAddr)
	if err != nil {
		return fmt.Errorf("Error resolving server address: %v", err)
	}

	c.conn, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("Error connecting to server: %v", err)
	}

	fmt.Println("UDP client started. Connected to", c.serverAddr)
	fmt.Println("Type messages and press Enter (type 'exit' to quit).")

	// Listen for server messages
	go c.listen()

	// Keep-alive
	go c.keepAlive()

	// Handle user input
	c.handleInput()

	return nil
}

// Listen for server replies
func (c *Client) listen() {
	buffer := make([]byte, 1024)
	for {
		n, _, err := c.conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading from server:", err)
			continue
		}
		fmt.Println("\n[Server]:", string(buffer[:n]))
		fmt.Print(">> ")
	}
}

// Send PING to keep connection alive
func (c *Client) keepAlive() {
	ticker := time.NewTicker(28 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		_, err := c.conn.Write([]byte("PING"))
		if err != nil {
			fmt.Println("Error sending keep-alive:", err)
		}
	}
}

// Handle console input
func (c *Client) handleInput() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print(">> ")
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(text)

		if text == "exit" {
			fmt.Println("Client exiting...")
			break
		}

		_, err := c.conn.Write([]byte(text))
		if err != nil {
			fmt.Println("Error sending:", err)
		}
	}
}

// ----------------- main -----------------
func main() {
	client := NewClient("173.208.144.109:10000")
	if err := client.Start(); err != nil {
		fmt.Println("Client error:", err)
	}
}

// package main

// import (
// 	"bufio"
// 	"fmt"
// 	"net"
// 	"os"
// 	"strings"
// )

// func main() {
// 	serverAddr, err := net.ResolveUDPAddr("udp", "localhost:8081")
// 	if err != nil {
// 		fmt.Println("Error resolving address:", err)
// 		return
// 	}

// 	conn, err := net.DialUDP("udp", nil, serverAddr)
// 	if err != nil {
// 		fmt.Println("Error dialing:", err)
// 		return
// 	}
// 	defer conn.Close()

// 	fmt.Println("UDP client started. Type messages and press Enter.")
// 	fmt.Println("Type 'exit' to quit.")

// 	reader := bufio.NewReader(os.Stdin)
// 	buffer := make([]byte, 1024)

// 	for {
// 		fmt.Print(">> ")
// 		text, _ := reader.ReadString('\n')
// 		text = strings.TrimSpace(text)

// 		if text == "exit" {
// 			fmt.Println("Client exiting...")
// 			break
// 		}

// 		_, err = conn.Write([]byte(text))
// 		if err != nil {
// 			fmt.Println("Error writing:", err)
// 			continue
// 		}

// 		n, _, err := conn.ReadFromUDP(buffer)
// 		if err != nil {
// 			fmt.Println("Error reading:", err)
// 			continue
// 		}

// 		fmt.Println("Server reply:", string(buffer[:n]))
// 	}
// }

//--------------------------------
// package main

// import (
// 	"fmt"
// 	"net"
// )

// func main() {
// 	serverAddr, _ := net.ResolveUDPAddr("udp", "localhost:8081")
// 	conn, _ := net.DialUDP("udp", nil, serverAddr)
// 	defer conn.Close()
// 	var msg string
// 	fmt.Scanln(&msg)
// 	_, _ = conn.Write([]byte(msg))

// 	buffer := make([]byte, 1024)
// 	n, _, _ := conn.ReadFromUDP(buffer)
// 	fmt.Println("Server replied:", string(buffer[:n]))
// }
