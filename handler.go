package main

import "sync"

var Handlers = map[string]func([]Value) Value{
	"PING":    ping,
	"SET":     set,
	"GET":     get,
	"DEL":     del,
	"COMMAND": command,
}

// writeCommands are the commands that change state, and so are the only ones
// worth appending to the AOF. Replaying a GET would be pointless work.
var writeCommands = map[string]bool{
	"SET": true,
	"DEL": true,
}

var SETs = map[string]string{}
var SETsMu = sync.RWMutex{}

func ping(args []Value) Value {
	if len(args) == 0 {
		return Value{typ: "string", str: "PONG"}
	}

	return Value{typ: "string", str: args[0].bulk}
}

// command answers the COMMAND / COMMAND DOCS handshake redis-cli sends on
// connect. An empty array is a valid "I have nothing to introspect" reply.
func command(args []Value) Value {
	return Value{typ: "array", array: []Value{}}
}

func set(args []Value) Value {
	if len(args) != 2 {
		return Value{typ: "error", str: "ERR wrong number of arguments for 'set' command"}
	}

	key := args[0].bulk
	value := args[1].bulk

	SETsMu.Lock()
	SETs[key] = value
	SETsMu.Unlock()

	return Value{typ: "string", str: "OK"}
}

func get(args []Value) Value {
	if len(args) != 1 {
		return Value{typ: "error", str: "ERR wrong number of arguments for 'get' command"}
	}

	key := args[0].bulk

	SETsMu.RLock()
	value, ok := SETs[key]
	SETsMu.RUnlock()

	if !ok {
		return Value{typ: "null"}
	}

	return Value{typ: "bulk", bulk: value}
}

// del removes one or more keys and returns how many actually existed.
func del(args []Value) Value {
	if len(args) == 0 {
		return Value{typ: "error", str: "ERR wrong number of arguments for 'del' command"}
	}

	deleted := 0

	SETsMu.Lock()
	for _, arg := range args {
		if _, ok := SETs[arg.bulk]; ok {
			delete(SETs, arg.bulk)
			deleted++
		}
	}
	SETsMu.Unlock()

	return Value{typ: "integer", num: deleted}
}
