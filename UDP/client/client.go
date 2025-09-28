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

func main() {
	serverAddr, err := net.ResolveUDPAddr("udp", "173.208.144.109:10000")
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		fmt.Println("Error dialing:", err)
		return
	}
	defer conn.Close()

	fmt.Println("UDP client started. Type messages and press Enter.")
	fmt.Println("Type 'exit' to quit.")

	go func() {
		buffer := make([]byte, 1024)
		for {
			n, _, err := conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading:", err)
				continue
			}
			fmt.Println("\n[Server reply]:", string(buffer[:n]))
			fmt.Print(">> ")
		}
	}()

	go func() {
		ticker := time.NewTicker(28 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			_, err := conn.Write([]byte("PING"))
			if err != nil {
				fmt.Println("Error sending keep-alive:", err)
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

		_, err = conn.Write([]byte(text))
		if err != nil {
			fmt.Println("Error writing:", err)
		}
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
