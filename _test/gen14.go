package main

import g1 "github.com/breadchris/yaegi/_test/g1"

func main() {
	a := [6]int{1, 2, 3, 4, 5, 6}
	m := g1.Map[int, int](a[:], func(item int, index int) int {
		return item * 2
	})
	for i := range m {
		println(m[i])
	}

	var TC int
	println(TC)
}

// Output:
// 2
// 4
// 6
// 8
// 10
// 12
// 0
