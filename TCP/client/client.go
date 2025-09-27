// TCP client
package main

import (
	"fmt"
	"net"
	"sync"
	"time"
)

func main() {
	var wg sync.WaitGroup
	numClients := 5

	for i := 1; i <= numClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runClient(id)
		}(i)
	}

	wg.Wait()
}

func runClient(id int) {
	conn, err := net.Dial("tcp", "localhost:8080")
	if err != nil {
		fmt.Printf("Client %d failed to connect: %v\n", id, err)
		return
	}
	defer conn.Close()
	fmt.Printf("Client %d connected to server\n", id)

	buffer := make([]byte, 1024)

	for i := 1; i <= 3; i++ {
		msg := fmt.Sprintf("Hello from client %d - message %d", id, i)
		_, err = conn.Write([]byte(msg))
		if err != nil {
			fmt.Printf("Client %d error sending: %v\n", id, err)
			return
		}

		n, err := conn.Read(buffer)
		if err != nil {
			fmt.Printf("Client %d error receiving: %v\n", id, err)
			return
		}

		fmt.Printf("Client %d got reply: %s\n", id, string(buffer[:n]))

		time.Sleep(1 * time.Second)
	}
}

// package main

// import (
// 	"fmt"
// 	"net"
// )

// func main() {
// 	conn, err := net.Dial("tcp", "localhost:8080")
// 	if err != nil {
// 		panic(err)
// 	}

// 	defer conn.Close()

// 	var msg string
// 	fmt.Scanln(&msg)

// 	_, err = conn.Write([]byte(msg))
// 	if err != nil {
// 		fmt.Println(err)
// 	}

// 	buffer := make([]byte, 1024)
// 	n, err := conn.Read(buffer)
// 	if err != nil {
// 		fmt.Println(err)
// 	}

// 	out := string(buffer[:n])
// 	fmt.Println(out)
// }
