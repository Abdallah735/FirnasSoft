package main

import "fmt"

func add(a, b int) (int, int) {
	return a + b, a - b
}

func main() {
	fmt.Println("hello Baby64")
	x, y := add(5, 4)
	fmt.Println(x, y)

}
