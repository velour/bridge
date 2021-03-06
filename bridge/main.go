// +build ignore

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/eaburns/pretty"
	"github.com/golang/sync/errgroup"
	"github.com/velour/chat"
	"github.com/velour/chat/bridge"
	"github.com/velour/chat/discord"
	"github.com/velour/chat/irc"
	"github.com/velour/chat/slack"
	"github.com/velour/chat/telegram"
)

var (
	telegramToken        = flag.String("telegram-token", "", "The bot's Telegram token")
	telegramGroup        = flag.String("telegram-group", "", "The bot's Telegram group ID")
	telegramNoWebPreview = flag.String("telegram-no-web-preview", "", "A regexp that prevents webpreview for sends if the text matches.")

	ircNick    = flag.String("irc-nick", "", "The bot's IRC nickname")
	ircPass    = flag.String("irc-password", "", "The bot's IRC password")
	ircServer  = flag.String("irc-server", "irc.freenode.net:6697", "The IRC server")
	ircChannel = flag.String("irc-channel", "#velour-test", "The IRC channel")

	slackToken = flag.String("slack-token", "", "The bot's Slack token")
	slackRoom  = flag.String("slack-room", "", "The bot's slack room name (not ID)")

	discordToken        = flag.String("discord-token", "", "The bot's Discord token")
	discordChannel      = flag.String("discord-channel", "", "Discord server_name:channel_name")
	discordRewriteNames = flag.String("discord-rewrite-names", "", "A comma-delimited list of <name>:<newname> pairs used to rewrite Discord display names.")

	httpPublic = flag.String("http-public", "http://localhost:8888", "The bridge's public base URL")
	httpServe  = flag.String("http-serve", "localhost:8888", "The bridge's HTTP server host")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	channels := []chat.Channel{}

	if *ircNick != "" {
		ircClient, err := irc.DialSSL(ctx, *ircServer, *ircNick, *ircNick, *ircPass, false)
		if err != nil {
			panic(err)
		}
		defer func() {
			if err := ircClient.Close(ctx); err != nil {
				panic(err)
			}
		}()
		ircChannel, err := ircClient.Join(ctx, *ircChannel)
		if err != nil {
			panic(err)
		}

		channels = append(channels, ircChannel)
	}

	if *telegramToken != "" {
		telegramClient, err := telegram.Dial(ctx, *telegramToken)
		if err != nil {
			panic(err)
		}
		defer func() {
			if err := telegramClient.Close(ctx); err != nil {
				panic(err)
			}
		}()
		telegramChannel, err := telegramClient.Join(ctx, *telegramGroup)
		if err != nil {
			panic(err)
		}

		if *telegramNoWebPreview != "" {
			re, err := regexp.Compile(*telegramNoWebPreview)
			if err != nil {
				panic(err)
			}
			telegramChannel.(interface {
				NoWebPreview(*regexp.Regexp)
			}).NoWebPreview(re)
		}

		const telegramMediaPath = "/telegram/media/"
		http.Handle(telegramMediaPath, telegramClient)
		baseURL, err := url.Parse(*httpPublic)
		if err != nil {
			panic(err)
		}
		baseURL.Path = path.Join(baseURL.Path, telegramMediaPath)
		telegramClient.SetLocalURL(*baseURL)

		channels = append(channels, telegramChannel)
	}

	if *slackToken != "" {
		slackClient, err := slack.Dial(ctx, *slackToken)
		if err != nil {
			panic(err)
		}
		defer func() {
			if err := slackClient.Close(ctx); err != nil {
				panic(err)
			}
		}()
		slackChannel, err := slackClient.Join(ctx, *slackRoom)
		if err != nil {
			panic(err)
		}
		channels = append(channels, slackChannel)
	}

	if *discordToken != "" {
		serverChan := strings.Split(*discordChannel, ":")
		if *discordChannel != "" && len(serverChan) != 2 {
			panic("malformed -discord-channel")
		}
		discordClient, err := discord.Dial(ctx, *discordToken)
		if err != nil {
			panic(err)
		}
		defer func() {
			if err := discordClient.Close(ctx); err != nil {
				panic(err)
			}
		}()

		for _, rs := range strings.Split(*discordRewriteNames, ",") {
			r := strings.Split(rs, ":")
			if len(r) != 2 {
				panic("invalid discord rewrite name: " + rs)
			}
			discordClient.RewriteName(r[0], r[1])
		}

		discordChan, err := discordClient.Join(ctx, serverChan[0], serverChan[1])
		if err != nil {
			panic(err)
		}
		channels = append(channels, discordChan)
	}

	go http.ListenAndServe(*httpServe, nil)

	b := bridge.New(channels...)
	log.Println("Bridge is up and running.")
	log.Println("Connecting:")
	for _, ch := range channels {
		log.Println("\t", ch.Name(), "on", ch.ServiceName())
	}
	if _, err := chat.Say(ctx, b, "Hello, World!"); err != nil {
		panic(err)
	}

	timeout := false
loop:
	for {
		ev, err := b.Receive(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		pretty.Print(ev)
		fmt.Print("\n")
		switch m := ev.(type) {
		case chat.Message:
			switch m.Text {
			case "LEAVE":
				if _, err := chat.Say(ctx, b, "Good bye!"); err != nil {
					panic(err)
				}
				break loop
			case "TIMEOUT":
				timeout = true
				if _, err := chat.Say(ctx, b, "Good bye for a bit…"); err != nil {
					panic(err)
				}
				break loop
			}
		}
	}
	if err := b.Close(ctx); err != nil {
		var group errgroup.Group
		for _, ch := range channels {
			ch := ch
			group.Go(func() error {
				chat.Say(ctx, ch, "Bridge closed with error: "+err.Error())
				return nil
			})
		}
		log.Printf("Bridge closed with error: %s", err)
		group.Wait()
	}
	if timeout {
		time.Sleep(10 * time.Minute)
	}
}

func whoTxt(users []chat.User) string {
	var txt string
	for _, u := range users {
		if len(txt) > 0 {
			txt += "\n"
		}
		txt += u.Name() +
			" in " + u.Channel.Name() +
			" on " + u.Channel.ServiceName()
	}
	return txt
}
