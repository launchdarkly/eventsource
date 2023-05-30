package eventsource_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/launchdarkly/eventsource"
)

type NewsArticle struct {
	id             string
	Title, Content string
}

func NewNewsArticle(id, title, content string) *NewsArticle {
	return &NewsArticle{
		id:      id,
		Title:   title,
		Content: content,
	}
}

func (a *NewsArticle) Id() string           { return a.id }
func (a *NewsArticle) Event() string        { return "News Article" }
func (a *NewsArticle) GetReader() io.Reader { b, _ := json.Marshal(a); return bytes.NewReader(b) }

var articles = []NewsArticle{
	*NewNewsArticle("2", "Governments struggle to control global price of gas", "Hot air...."),
	*NewNewsArticle("1", "Tomorrow is another day", "And so is the day after."),
	*NewNewsArticle("3", "News for news' sake", "Nothing has happened."),
}

func buildRepo(srv *eventsource.Server) {
	repo := eventsource.NewSliceRepository()
	srv.Register("articles", repo)
	for i := range articles {
		repo.Add("articles", &articles[i])
		srv.Publish([]string{"articles"}, &articles[i])
	}
}

func ExampleRepository() {
	srv := eventsource.NewServer()
	defer srv.Close()
	http.HandleFunc("/articles", srv.Handler("articles"))
	l, err := net.Listen("tcp", ":8080")
	if err != nil {
		return
	}
	defer l.Close()
	go http.Serve(l, nil)
	stream, err := eventsource.Subscribe("http://127.0.0.1:8080/articles", "")
	if err != nil {
		return
	}
	go buildRepo(srv)
	// This will receive events in the order that they come
	for i := 0; i < 3; i++ {
		ev := <-stream.Events
		evData, err := io.ReadAll(ev.GetReader())
		if err == nil {
			fmt.Println(ev.Id(), ev.Event(), string(evData))
		}
	}
	stream, err = eventsource.Subscribe("http://127.0.0.1:8080/articles", "1")
	if err != nil {
		fmt.Println(err)
		return
	}
	// This will replay the events in order of id
	for i := 0; i < 3; i++ {
		ev := <-stream.Events
		evData, err := io.ReadAll(ev.GetReader())
		if err == nil {
			fmt.Println(ev.Id(), ev.Event(), string(evData))
		}
	}
	// Output:
	// 2 News Article {"Title":"Governments struggle to control global price of gas","Content":"Hot air...."}
	// 1 News Article {"Title":"Tomorrow is another day","Content":"And so is the day after."}
	// 3 News Article {"Title":"News for news' sake","Content":"Nothing has happened."}
	// 1 News Article {"Title":"Tomorrow is another day","Content":"And so is the day after."}
	// 2 News Article {"Title":"Governments struggle to control global price of gas","Content":"Hot air...."}
	// 3 News Article {"Title":"News for news' sake","Content":"Nothing has happened."}
}
