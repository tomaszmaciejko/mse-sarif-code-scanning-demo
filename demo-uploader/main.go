package main

import (
 "context"
 "fmt"
 "os"
)

func main() {
 if err := runUpload(context.Background(),os.Args[1:],os.Stdout); err != nil {
  fmt.Fprintln(os.Stderr,err)
  os.Exit(1)
 }
}
