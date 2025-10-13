package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

func getUDPRecvQueueSize(conn *net.UDPConn) (int, error) {
	file, err := conn.File()
	if err != nil {
		return 0, err
	}
	defer file.Close()

	fd := syscall.Handle(file.Fd())
	size, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func getUDPSendQueueSize(conn *net.UDPConn) (int, error) {
	file, err := conn.File()
	if err != nil {
		return 0, err
	}
	defer file.Close()

	fd := syscall.Handle(file.Fd())
	size, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	if err != nil {
		return 0, err
	}
	return size, nil
}

func main() {
	addr, _ := net.ResolveUDPAddr("udp", ":9000")
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
	defer conn.Close()

	rsize, _ := getUDPRecvQueueSize(conn)
	ssize, _ := getUDPSendQueueSize(conn)

	fmt.Printf("Receive buffer size: %d bytes\n", rsize)
	fmt.Printf("Send buffer size: %d bytes\n", ssize)
}
