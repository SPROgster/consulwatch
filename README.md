# consulwatch

consulwatch is a Go library for watching [Consul](https://www.consul.io) KV values.
3 state change: create, update, delete with channel separation or without

## Installation

Standard `go get`:

```
$ go get github.com/mitchellh/consulstructure
```

## Usage & Example

```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	cw "github.com/SPROgster/consulwatch"
)

func main() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)

	cr := make(chan map[string]interface{})
	de := make(chan map[string]interface{})
	upd:= make(chan map[string]interface{})

	watched := cw.ConsulWatched{
		Prefix: "someprefix",
		CreateChan: cr,
		DeleteChan: de,
		UpdateChan: upd,
	}

	err := watched.Watch()
	if err != nil {
		log.Fatal(err)
	}
	defer watched.Stop()

	for {
		select {
		case <-sigs:
			fmt.Println("Stopping")
			return
		case a := <-watched.UpdateChan:
			fmt.Printf("updated: %v\n", a)
		case a := <-watched.CreateChan:
			fmt.Printf("created: %v\n", a)
		case a := <-watched.DeleteChan:
			fmt.Printf("deleted: %v\n", a)
		}
	}
}
```

If no different channels needed, consilwatch allows notifications via update channel.
Deleted values has nil in interface{}.
In this case, update chan will be created in inside Watch() function

Example:

```go
watched := cw.ConsulWatched{
		Prefix: "someprefix",
	}
```