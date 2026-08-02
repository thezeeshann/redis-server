package redis

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// execMu serializes command execution across connections, so the order of
// commands in the AOF always matches the order they were applied in memory.
// Real Redis gets this for free by being single threaded.
var execMu sync.Mutex

// ListenAndServe opens the append-only file at aofPath, replays it to rebuild
// the dataset, then serves clients on address until the process is interrupted.
func ListenAndServe(address, aofPath string) error {
	aof, err := NewAof(aofPath)
	if err != nil {
		return fmt.Errorf("opening AOF: %w", err)
	}
	defer aof.Close()

	// Rebuild state from the log before accepting any client.
	if err := replay(aof); err != nil {
		return fmt.Errorf("replaying AOF: %w", err)
	}

	l, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-shutdown
		fmt.Println("\nShutting down, flushing AOF...")
		l.Close()
	}()

	fmt.Println("Listening on", address)

	for {
		conn, err := l.Accept()
		if err != nil {
			// Accept fails once the listener is closed by the signal handler,
			// which is the normal shutdown path rather than a real error.
			return nil
		}

		go handleConn(conn, aof)
	}
}

// replay re-executes every command stored in the AOF.
func replay(aof *Aof) error {
	return aof.Read(func(value Value) {
		if value.typ != "array" || len(value.array) == 0 {
			return
		}

		command := strings.ToUpper(value.array[0].bulk)

		handler, ok := Handlers[command]
		if !ok {
			fmt.Println("Skipping unknown command in AOF:", command)
			return
		}

		handler(value.array[1:])
	})
}

func handleConn(conn net.Conn, aof *Aof) {
	defer conn.Close()

	fmt.Println("Client connected", conn.RemoteAddr())
	defer fmt.Println("Client disconnected", conn.RemoteAddr())

	resp := NewResp(conn)
	writer := NewWriter(conn)

	for {
		value, err := resp.Read()
		if err != nil {
			return
		}

		if value.typ != "array" || len(value.array) == 0 {
			writer.Write(Value{typ: "error", str: "ERR expected a non-empty array"})
			continue
		}

		// Lookup is case-insensitive, but the error quotes what was typed,
		// the way real Redis does.
		raw := value.array[0].bulk
		command := strings.ToUpper(raw)
		args := value.array[1:]

		handler, ok := Handlers[command]
		if !ok {
			writer.Write(Value{typ: "error", str: fmt.Sprintf("ERR unknown command '%s'", raw)})
			continue
		}

		execMu.Lock()

		// Append before applying, so a crash can never leave a change that is
		// live in memory but missing from the log.
		if writeCommands[command] {
			if err := aof.Write(value); err != nil {
				execMu.Unlock()
				writer.Write(Value{typ: "error", str: "ERR failed to persist command"})
				continue
			}
		}

		result := handler(args)

		execMu.Unlock()

		writer.Write(result)
	}
}
