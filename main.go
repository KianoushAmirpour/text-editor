package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

type Editor struct {
	EditType string `json:"edit_type"`       // insert | delete | replace
	Index    int    `json:"index"`           // where to apply edits
	Count    int    `json:"count,omitempty"` // count is used for delete or replace. how many index to delete? how many index to replace?
	Text     string `json:"text,omitempty"`  // text to be inserted or replaces
}

type Message struct {
	Action   string  `json:"action"` // edit | chat | sync_req
	DocID    string  `json:"doc_id"`
	UserName string  `json:"User_name,omitempty"`
	Edit     *Editor `json:"edit,omitempty"`
	Text     string  `json:"text,omitempty"`
	Version  int     `json:"version,omitempty"`
}

type Client struct {
	UserName string
	Conn     *websocket.Conn
	Send     chan Message
	Mu       sync.Mutex
	DocID    string
	Server   *Server
}

type Document struct {
	ID      string
	Mu      sync.Mutex
	Text    string
	Version int
	Clients map[*Client]bool
}

type Server struct {
	redis     *redis.Client
	Documents map[string]*Document
	ctx       context.Context
	Mu        sync.Mutex
}

func NewServer(rdb *redis.Client, ctx context.Context) *Server {
	return &Server{
		redis:     rdb,
		Documents: make(map[string]*Document),
		ctx:       ctx,
	}
}

func (s *Server) GetOrCreateDocument(docid string) *Document {

	document, ok := s.Documents[docid]
	if ok {
		return document
	}

	document = &Document{
		ID:      docid,
		Text:    "",
		Version: 1,
		Clients: make(map[*Client]bool),
	}

	s.Documents[docid] = document

	go s.SubscribeToDocumentChannel(docid)

	return document
}

func (s *Server) SubscribeToDocumentChannel(docid string) {
	channelName := fmt.Sprintf("doc:%s:edits", docid)
	sub := s.redis.Subscribe(s.ctx, channelName)
	ch := sub.Channel()
	for {
		for msg := range ch {
			var m Message
			err := json.Unmarshal([]byte(msg.Payload), &m)
			if err != nil {
				log.Printf("invalid message received from redis. Error: %v. Payload:%s", err, msg.Payload)
			}
			doc := s.GetOrCreateDocument(docid)
			for client := range doc.Clients {
				select {
				case client.Send <- m:
				default:
					go client.Conn.Close()
				}
			}
		}
	}
}

func (s *Server) PublishToDocumentChannel(docid string, msg Message) error {
	channelName := fmt.Sprintf("doc:%s:edits", docid)
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("failed to convert marshal. Error: %v. Message: %s", err, msg)
		return err
	}
	err = s.redis.Publish(s.ctx, channelName, b).Err()
	return err
}

func (s *Server) wsHandler(w http.ResponseWriter, r *http.Request) {
	docID := r.URL.Query().Get("docid")
	if docID == "" {
		http.Error(w, "missing doc id", http.StatusBadRequest)
		return
	}
	userName := r.URL.Query().Get("user_name")
	if userName == "" {
		userName = "guest"
	}

	Upgrader := websocket.Upgrader{
		ReadBufferSize:  1024 * 1024,
		WriteBufferSize: 1024 * 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	conn, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrader error", err)
		return
	}

	client := &Client{UserName: userName, Conn: conn, Send: make(chan Message, 32), Server: s, DocID: docID}

	doc := s.GetOrCreateDocument(docID)
	doc.Mu.Lock()
	doc.Clients[client] = true
	doc.Mu.Unlock()

	m := Message{
		Action:  "sync_req",
		DocID:   docID,
		Text:    doc.Text,
		Version: doc.Version,
	}

	client.Send <- m

	go client.Write()
	go client.Read()
}

func (c *Client) Read() {
	defer func() {
		c.Server.RemovedClients(c)
		c.Conn.Close()
	}()

	for {
		var m Message
		err := c.Conn.ReadJSON(&m)
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			log.Println("failed to read from websocket", err)
			return
		}
		m.UserName = c.UserName
		m.DocID = c.DocID

		switch m.Action {
		case "chat":
			_ = c.Server.PublishToDocumentChannel(c.DocID, m)
		case "edit":
			if m.Edit == nil {
				log.Println("edit message missing.")
				continue
			}
			err = c.Server.applyEditandPublish(c.DocID, m)
			if err != nil {
				log.Println("failed to apply edits and publish", err)
			}
		case "sync_req":
			doc := c.Server.GetOrCreateDocument(c.DocID)
			c.Send <- Message{
				Action:  "full_sync",
				DocID:   c.DocID,
				Text:    doc.Text,
				Version: doc.Version,
			}
		default:
			log.Println("unknow action", m.Action)
		}

	}

}

func (c *Client) Write() {
	for m := range c.Send {
		err := c.Conn.WriteMessage(websocket.TextMessage, []byte(m.Text))
		if err != nil {
			log.Println("failed to write in websocket", err)
			c.Conn.Close()
			return
		}
	}
}

func (s *Server) RemovedClients(c *Client) {
	doc := s.GetOrCreateDocument(c.DocID)
	doc.Mu.Lock()
	delete(doc.Clients, c)
	doc.Mu.Unlock()
	close(c.Send)
}

func (s *Server) applyEditandPublish(docid string, m Message) error {
	doc := s.GetOrCreateDocument(docid)
	newText, err := applyEdit(doc.Text, m.Edit)
	if err != nil {
		return err
	}
	doc.Text = newText
	doc.Version++
	m.Version = doc.Version

	if err := s.PublishToDocumentChannel(docid, m); err != nil {
		log.Println("failed to publish to redis", err)
		return err
	}
	return nil
}

func applyEdit(origin string, edit *Editor) (string, error) {
	if edit == nil {
		return origin, fmt.Errorf("nil edit type")
	}
	switch edit.EditType {
	case "insert":
		if edit.Index < 0 {
			return origin, fmt.Errorf("negative index")
		}
		runes := []rune(origin)
		if edit.Index > len(runes) {
			return origin, fmt.Errorf("index out of bound")
		}
		left := string(runes[:edit.Index])
		right := string(runes[edit.Index:])
		return left + edit.Text + right, nil
	case "delete":
		if edit.Count <= 0 {
			return origin, fmt.Errorf("delete count must be > 0")
		}
		runes := []rune(origin)
		if edit.Index < 0 || edit.Index >= len(runes) {
			return origin, fmt.Errorf("index out of bounds")
		}
		end := edit.Count + edit.Index
		if end > len(runes) {
			end = len(runes)
		}
		left := string(runes[:edit.Index])
		right := string(runes[end:])
		return left + right, nil
	case "replace":
		runes := []rune(origin)
		if edit.Index < 0 || edit.Index >= len(runes) {
			return origin, fmt.Errorf("index out of bounds")
		}
		end := edit.Count + edit.Index
		if end > len(runes) {
			end = len(runes)
		}

		left := string(runes[:edit.Index])
		right := string(runes[end:])
		return left + edit.Text + right, nil
	default:
		return origin, fmt.Errorf("unknown edit type: %s", edit.EditType)
	}

}

func ConnectToRedis(addr string, database int) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   database,
	})

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Minute*10)
	defer cancelFunc()

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return nil, err
	}

	return rdb, nil

}

func main() {
	rdb, err := ConnectToRedis("localhost:6379", 0)
	if err != nil {
		panic(err)
	}
	s := NewServer(rdb, context.Background())
	http.HandleFunc("/ws", s.wsHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
