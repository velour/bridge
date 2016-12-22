package irc

import (
	"bufio"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/velour/bridge/chat"
)

var _ chat.Client = &Client{}

// A Client is a client's connection to an IRC server.
type Client struct {
	conn  net.Conn
	in    *bufio.Reader
	error chan error

	sync.Mutex
	nick     string
	channels map[string]*channel
}

// Dial connects to a remote IRC server.
func Dial(server, nick, fullname, pass string) (*Client, error) {
	c, err := net.Dial("tcp", server)
	if err != nil {
		return nil, err
	}
	return dial(c, nick, fullname, pass)
}

// DialSSL connects to a remote IRC server using SSL.
func DialSSL(server, nick, fullname, pass string, trust bool) (*Client, error) {
	c, err := tls.Dial("tcp", server, &tls.Config{InsecureSkipVerify: trust})
	if err != nil {
		return nil, err
	}
	return dial(c, nick, fullname, pass)
}

func dial(conn net.Conn, nick, fullname, pass string) (*Client, error) {
	c := &Client{
		conn:     conn,
		in:       bufio.NewReader(conn),
		error:    make(chan error),
		nick:     nick,
		channels: make(map[string]*channel),
	}
	if err := register(c, nick, fullname, pass); err != nil {
		return nil, err
	}
	go poll(c)
	return c, nil
}

func register(c *Client, nick, fullname, pass string) error {
	if pass != "" {
		if err := send(c, PASS, pass); err != nil {
			return err
		}
	}
	if err := send(c, NICK, nick); err != nil {
		return err
	}
	if err := send(c, USER, nick, "0", "*", fullname); err != nil {
		return err
	}
	for {
		msg, err := next(c)
		if err != nil {
			return err
		}
		switch msg.Command {
		case ERR_NONICKNAMEGIVEN, ERR_ERRONEUSNICKNAME,
			ERR_NICKNAMEINUSE, ERR_NICKCOLLISION,
			ERR_UNAVAILRESOURCE, ERR_RESTRICTED,
			ERR_NEEDMOREPARAMS, ERR_ALREADYREGISTRED:
			if len(msg.Arguments) > 0 {
				return errors.New(msg.Arguments[len(msg.Arguments)-1])
			}
			return errors.New(CommandNames[msg.Command])

		case RPL_WELCOME:
			return nil

		default:
			/* ignore */
		}
	}
}

// Close closes the connection.
func (c *Client) Close() error {
	send(c, QUIT)
	closeErr := c.conn.Close()
	pollErr := <-c.error
	for _, ch := range c.channels {
		close(ch.in)
	}
	if closeErr != nil {
		return closeErr
	}
	return pollErr
}

func send(c *Client, cmd string, args ...string) error {
	msg := Message{Command: cmd, Arguments: args}
	deadline := time.Now().Add(time.Minute)
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	bs := msg.Bytes()
	if len(bs) > MaxBytes {
		return TooLongError{Message: bs[:MaxBytes], NTrunc: len(bs) - MaxBytes}
	}
	_, err := c.conn.Write(bs)
	return err
}

// next returns the next message from the server.
// It never returns a PING command;
// the client responds to PINGs automatically.
func next(c *Client) (Message, error) {
	for {
		switch msg, err := read(c.in); {
		case err != nil:
			return Message{}, err
		case msg.Command == PING:
			if err := send(c, PONG, msg.Arguments...); err != nil {
				return Message{}, err
			}
		default:
			return msg, nil
		}
	}
}

func poll(c *Client) {
	var err error
loop:
	for {
		var msg Message
		if msg, err = next(c); err != nil {
			break loop
		}
		switch msg.Command {
		case JOIN:
			if len(msg.Arguments) < 1 {
				log.Printf("Received bad JOIN: %+v\n", msg)
				continue
			}
			channelName := msg.Arguments[0]
			c.Lock()
			ch, ok := c.channels[channelName]
			myNick := c.nick
			c.Unlock()
			if !ok {
				log.Printf("Unknown channel %s received WHOREPLY", channelName)
				continue
			}
			if msg.Origin == myNick {
				continue
			}
			ch.Lock()
			ch.users[msg.Origin] = true
			ch.Unlock()
			sendEvent(c, channelName, &msg, chat.Join{Who: chatUser(msg.Origin)})

		case PART:
			if len(msg.Arguments) < 1 {
				log.Printf("Received bad PART: %+v\n", msg)
				continue
			}
			channelName := msg.Arguments[0]
			c.Lock()
			ch, ok := c.channels[channelName]
			myNick := c.nick
			c.Unlock()
			if !ok {
				log.Printf("Unknown channel %s received WHOREPLY", channelName)
				continue
			}
			if msg.Origin == myNick {
				continue
			}
			ch.Lock()
			delete(ch.users, msg.Origin)
			ch.Unlock()
			sendEvent(c, channelName, &msg, chat.Leave{Who: chatUser(msg.Origin)})

		case NICK:
			if len(msg.Arguments) < 2 {
				log.Printf("Received bad NICK: %+v\n", msg)
				continue
			}
			rename := chat.Rename{Who: chatUser(msg.Origin)}
			// Fake that the ID is their original nick, before the change.
			rename.Who.ID = chat.UserID(msg.Arguments[1])

			c.Lock()
			if msg.Arguments[1] == c.nick {
				// The bot's nick was changed.
				c.nick = msg.Origin
			}
			for channelName, ch := range c.channels {
				ch.Lock()
				if ch.users[msg.Origin] {
					delete(ch.users, msg.Origin)
					ch.users[msg.Arguments[0]] = true
					sendEventLocked(c, channelName, &msg, rename)
				}
				ch.Unlock()
			}
			c.Unlock()

		case QUIT:
			leave := chat.Leave{Who: chatUser(msg.Origin)}
			c.Lock()
			for channelName, ch := range c.channels {
				ch.Lock()
				if ch.users[msg.Origin] {
					delete(ch.users, msg.Origin)
					sendEventLocked(c, channelName, &msg, leave)
				}
				ch.Unlock()
			}
			c.Unlock()

		case PRIVMSG:
			if len(msg.Arguments) < 2 {
				log.Printf("Received bad PRIVMSG: %+v\n", msg)
				continue
			}
			text := msg.Arguments[1]
			message := chat.Message{
				ID:   chat.MessageID(text),
				From: chatUser(msg.Origin),
				Text: text,
			}
			sendEvent(c, msg.Arguments[0], &msg, message)

		case RPL_WHOREPLY:
			if len(msg.Arguments) < 6 {
				log.Printf("Received bad WHOREPLY: %+v\n", msg)
				continue
			}
			channelName := msg.Arguments[1]
			nick := msg.Arguments[5]
			c.Lock()
			ch, ok := c.channels[channelName]
			myNick := c.nick
			c.Unlock()
			if !ok {
				log.Printf("Unknown channel %s received WHOREPLY", channelName)
				continue
			}
			if nick == myNick {
				continue
			}
			select {
			case ch.inWho <- []string{nick}:
			case ns := <-ch.inWho:
				ch.inWho <- append(ns, nick)
			}

		case RPL_ENDOFWHO:
			if len(msg.Arguments) < 2 {
				log.Printf("Received bad ENDOFWHO: %+v\n", msg)
				continue
			}
			channelName := msg.Arguments[1]
			c.Lock()
			ch, ok := c.channels[channelName]
			c.Unlock()
			if !ok {
				log.Printf("Unknown channel %s received WHOREPLY", channelName)
				return
			}
			close(ch.inWho)
		}
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		// If the error was 'use of closed network connection', the user called Client.Close.
		// It's not an error.
		err = nil
	}
	c.error <- err
}

func chatUser(nick string) chat.User {
	return chat.User{ID: chat.UserID(nick), Nick: nick, Name: nick}
}

func sendEvent(c *Client, channelName string, msg *Message, event interface{}) {
	c.Lock()
	defer c.Unlock()
	sendEventLocked(c, channelName, msg, event)
}

// Just like sendEvent, but assumes that c.Lock is held.
func sendEventLocked(c *Client, channelName string, msg *Message, event interface{}) {
	ch, ok := c.channels[channelName]
	if !ok {
		log.Printf("Unknown channel %s received message %+v", channelName, msg)
		return
	}
	select {
	case ch.in <- []interface{}{event}:
	case es := <-ch.in:
		ch.in <- append(es, event)
	}
}

func (c *Client) Join(channelName string) (chat.Channel, error) {
	c.Lock()
	defer c.Unlock()
	if ch, ok := c.channels[channelName]; ok {
		return ch, nil
	}

	// JOIN and WHO happen with c.Lock held.
	// Maybe it's bad practice to do network sends with a lock held,
	// but it guarantees that everything on c.channels is JOINed.
	// In otherwords, we should never receive a message
	// for a channel that is not already on c.channels.
	if err := send(c, JOIN, channelName); err != nil {
		return nil, err
	}
	if err := send(c, WHO, channelName); err != nil {
		return nil, err
	}
	ch := newChannel(c, channelName)
	c.channels[channelName] = ch
	return ch, nil
}