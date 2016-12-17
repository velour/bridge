// +build ignore

// Package main is a demo to "test" the Telegram bot Client API.
package main

import (
	"flag"
	"fmt"

	"github.com/eaburns/pretty"
	"github.com/velour/bridge/chat/telegram"
)

var token = flag.String("token", "", "The bot's token")

func main() {
	flag.Parse()
	c, err := telegram.New(*token)
	if err != nil {
		panic(err)
	}
	pretty.Print(c.Me())
	fmt.Println("")

	for u := range c.Updates() {
		pretty.Print(u)
		fmt.Println("")
	}
	panic(c.Err())
}
