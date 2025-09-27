// UDP server
package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	addr, err := net.ResolveUDPAddr("udp", "173.208.144.109:10000")
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error listening:", err)
		return
	}
	defer conn.Close()

	fmt.Println("UDP server listening on port 10000")

	clients := make(map[string]*net.UDPAddr)
	var mu sync.Mutex

	go func() {
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
				targetAddr := parts[1]
				message := parts[2]

				mu.Lock()
				client, ok := clients[targetAddr]
				mu.Unlock()
				if !ok {
					fmt.Println("Client not found:", targetAddr)
					continue
				}

				_, err := conn.WriteToUDP([]byte("[Server] "+message), client)
				if err != nil {
					fmt.Println("Error sending to", targetAddr, ":", err)
				} else {
					fmt.Println("Message sent to", targetAddr)
				}
			} else if line == "list" {
				mu.Lock()
				for c := range clients {
					fmt.Println("Client:", c)
				}
				mu.Unlock()
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			for _, client := range clients {
				conn.WriteToUDP([]byte("I remember you"), client)
			}
			mu.Unlock()
		}
	}()

	buffer := make([]byte, 1024)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			continue
		}

		mu.Lock()
		clients[clientAddr.String()] = clientAddr
		mu.Unlock()

		data := make([]byte, n)
		copy(data, buffer[:n])

		go func(pkt []byte, addr *net.UDPAddr) {
			msg := string(pkt)
			fmt.Printf("Received from %v: %s\n", addr, msg)

			if msg == "PING" {
				conn.WriteToUDP([]byte("PONG"), addr)
				return
			}

			reply := fmt.Sprintf("Echo: %s", msg)
			_, err := conn.WriteToUDP([]byte(reply), addr)
			if err != nil {
				fmt.Println("Error writing to", addr, ":", err)
			}
		}(data, clientAddr)
	}
}

// package main
// import (
// 	"fmt"
// 	"net"
// )

// func main() {
// 	addr, err := net.ResolveUDPAddr("udp", ":8081")
// 	if err != nil {
// 		fmt.Println(err)
// 	}
// 	conn, _ := net.ListenUDP("udp", addr)
// 	defer conn.Close()
// 	fmt.Println("UDP server listening on port 8081")

// 	buffer := make([]byte, 1024)
// 	for {
// 		n, clientAddr, _ := conn.ReadFromUDP(buffer)
// 		fmt.Println("Received:", string(buffer[:n]))

// 		_, _ = conn.WriteToUDP([]byte(string(buffer[:n])), clientAddr)
// 	}
// }
