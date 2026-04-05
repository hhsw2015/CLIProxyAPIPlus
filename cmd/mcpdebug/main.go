package main
import (
	"os"
)
func main() {
	_ = os.WriteFile("/tmp/dummy", []byte(""), 0644)
}
