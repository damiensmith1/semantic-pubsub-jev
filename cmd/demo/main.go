// Command demo exercises semantic routing end to end.
//
// It connects three subscribers with different natural-language
// interests, publishes three messages that each match exactly one of
// them, and prints who received what. Run the server first:
//
//	redis-server --port 6399 --save '' --daemonize yes
//	LISTEN_ADDR=:8123 REDIS_ADDRS=127.0.0.1:6399 go run .
//	go run ./cmd/demo 127.0.0.1:8123
//
// With TYPESAFE_API_KEY set this routes through Jev; without it, through
// the keyword judge, which will not separate these messages as cleanly.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func dial(addr, user string) *websocket.Conn {
	c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws?userKey="+user, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	return c
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo <host:port>")
		os.Exit(2)
	}
	addr := os.Args[1]

	type sub struct {
		name, predicate string
		conn            *websocket.Conn
		got             []string
	}
	subs := []*sub{
		{name: "alice", predicate: "database and storage problems, including disk capacity"},
		{name: "bob", predicate: "network latency and connectivity issues"},
		{name: "carol", predicate: "security incidents and authentication failures"},
	}

	for _, s := range subs {
		s.conn = dial(addr, s.name)
		_ = s.conn.WriteJSON(map[string]any{"type": "subscribe", "topic": "alerts"})
		_ = s.conn.WriteJSON(map[string]any{
			"type": "interest", "topic": "alerts",
			"data": map[string]any{"predicate": s.predicate},
		})
		go func(s *sub) {
			for {
				_, raw, err := s.conn.ReadMessage()
				if err != nil {
					return
				}
				var f struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				}
				if json.Unmarshal(raw, &f) == nil && f.Type == "publish" {
					s.got = append(s.got, string(f.Data))
				}
			}
		}(s)
	}
	time.Sleep(1500 * time.Millisecond)

	pub := dial(addr, "publisher")
	messages := []map[string]any{
		{"service": "orders-db", "severity": "critical", "text": "primary volume at 96% capacity, writes will fail within the hour"},
		{"service": "edge-proxy", "severity": "warning", "text": "p99 round-trip time to eu-west up 400ms, packet loss 2%"},
		{"service": "auth-api", "severity": "critical", "text": "4,000 failed login attempts from a single ASN in 10 minutes"},
	}
	for _, m := range messages {
		_ = pub.WriteJSON(map[string]any{"type": "publish", "topic": "alerts", "data": m})
		time.Sleep(1200 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)

	fmt.Println()
	for i, m := range messages {
		fmt.Printf("message %d: %s\n", i+1, m["text"])
	}
	fmt.Println()
	for _, s := range subs {
		fmt.Printf("%-6s (%s)\n", s.name, s.predicate)
		if len(s.got) == 0 {
			fmt.Println("         received nothing")
		}
		for _, g := range s.got {
			var d map[string]any
			_ = json.Unmarshal([]byte(g), &d)
			fmt.Printf("         <- %v\n", d["service"])
		}
	}
}
