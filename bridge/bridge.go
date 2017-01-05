// Package bridge is a chat.Channel that acts as a relay,
// bridging multiple chat.Channels into a single, logical one.
//
// A Bridge is created with a slice of other chat.Channels, called the bridged channels.
// Events sent on a bridged channel are relayed to all other channels
// and are also returned by the Bridge.Receive method.
//
// The send-style methods of chat.Channel (Send, Delete, Edit, and so on)
// are forwarded to all bridged channels.
// In this way, the Bridge itself is a chat.Channel.
// This is useful, for example, to implement a chat bot
// that also bridges channels on multiple chat clients.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/golang/sync/errgroup"
	"github.com/velour/chat"
)

const maxHistory = 500

var _ chat.Channel = &Bridge{}

// A Bridge is a chat.Channel that bridges multiple channels.
// Events sent on each bridged channel are:
// 1) relayed to all other channels in the bridge, and
// 2) multiplexed to the Bridge.Receive method.
type Bridge struct {
	// eventsMux multiplexes events incoming from the bridged channels.
	eventsMux chan event

	// recvIn simulates an infinite buffered channel
	// of events, multiplexed from the bridged channels.
	// The mux goroutine publishes events to this channel without blocking.
	// The recv goroutine forwards the events to recvOut.
	recvIn chan []interface{}

	// recvOut publishes evetns to the Receive method.
	recvOut chan interface{}

	// pollError reports errors from the channel polling goroutines to the mux goroutine.
	// If the mux goroutine recieves a pollError, it forwards the error to closeError,
	// cancels all background goroutines, and returns.
	pollError chan error

	// closeError reports errors to the Close method.
	// The mux goroutine publishes to this channel, either forwarding an error
	// from pollError or by simply closing it without an error on successful Close.
	closeError chan error

	// closed is closed when the Close method is called.
	// On a clean close, this signals the mux goroutine to
	// cancel all background goroutines and close closeError.
	closed chan struct{}

	// channels are the channels being bridged.
	channels []chat.Channel

	sync.Mutex

	// nextID is the next ID for messages sent by the bridge.
	nextID int

	// log is a history of messages sent with or relayed by the bridge.
	log []*logEntry
}

type logEntry struct {
	origin chat.Channel
	copies []message
}

type message struct {
	to  chat.Channel
	msg chat.Message
}

// New returns a new bridge that bridges a set of channels.
func New(channels ...chat.Channel) *Bridge {
	b := &Bridge{
		eventsMux:  make(chan event, 100),
		recvIn:     make(chan []interface{}, 1),
		recvOut:    make(chan interface{}),
		pollError:  make(chan error, 1),
		closeError: make(chan error, 1),
		closed:     make(chan struct{}),
		channels:   channels,
	}

	// Polling goroutines run in the background;
	// they are cancelled when the done channel is closed.
	ctx, cancel := context.WithCancel(context.Background())
	for _, ch := range channels {
		go poll(ctx, b, ch)
	}
	go recv(ctx, b)
	go mux(ctx, cancel, b)
	return b
}

func (b *Bridge) Name() string        { return "bridge" }
func (b *Bridge) ServiceName() string { return "bridge" }

// Closes stops bridging the channels, closes the bridge.
func (b *Bridge) Close(ctx context.Context) error {
	close(b.closed)
	err := <-b.closeError
	if err == io.EOF {
		err = errors.New("unexpected EOF")
	}
	return err
}

type event struct {
	origin chat.Channel
	what   interface{}
}

// mux multiplexes:
// events incoming from bridged channels,
// errors coming from channel polling,
// and closing the bridge.
func mux(ctx context.Context, cancel context.CancelFunc, b *Bridge) {
	defer cancel()
	defer close(b.closeError)
	for {
		select {
		case <-b.closed:
			return
		case err := <-b.pollError:
			b.closeError <- err
			return
		case ev := <-b.eventsMux:
			if err := relay(ctx, b, ev); err != nil {
				b.closeError <- err
				return
			}
			select {
			case b.recvIn <- []interface{}{ev.what}:
			case evs := <-b.recvIn:
				b.recvIn <- append(evs, ev.what)
			}
		}
	}
}

// recv forwards events to the Receive method.
// If the context is canceled, unreceived events are dropped.
func recv(ctx context.Context, b *Bridge) {
	defer close(b.recvOut)
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-b.recvIn:
			for _, ev := range evs {
				select {
				case <-ctx.Done():
					return
				case b.recvOut <- ev:
				}
			}
		}
	}

}

func poll(ctx context.Context, b *Bridge, ch chat.Channel) {
	for {
		switch ev, err := ch.Receive(ctx); {
		case err == context.Canceled || err == context.DeadlineExceeded:
			// Ignore context errors. These are expected. No need to report back.
			return
		case err != nil:
			// Don't block. We only report the first error.
			select {
			case b.pollError <- err:
			default:
			}
			return
		default:
			b.eventsMux <- event{origin: ch, what: ev}
		}
	}
}

func logMessage(b *Bridge, entry *logEntry) {
	b.Lock()
	b.log = append(b.log, entry)
	if len(b.log) > maxHistory {
		b.log = b.log[:maxHistory]
	}
	b.Unlock()
}

func relay(ctx context.Context, b *Bridge, event event) error {
	origName := event.origin.Name() + " on " + event.origin.ServiceName()
	switch ev := event.what.(type) {
	case chat.Message:
		var err error
		to := allChannelsExcept(b, event.origin)
		msgs, err := sendMessage(ctx, to, &ev.From, nil, ev.Text)
		if err != nil {
			return err
		}
		msgs = append(msgs, message{to: event.origin, msg: ev})
		logMessage(b, &logEntry{origin: event.origin, copies: msgs})
		return nil

	case chat.Reply:
		findMessage := makeFindMessage(b, event.origin, ev.ReplyTo.ID)
		to := allChannelsExcept(b, event.origin)
		msgs, err := sendMessage(ctx, to, nil, findMessage, ev.Reply.Text)
		if err != nil {
			return err
		}
		msgs = append(msgs, message{to: event.origin, msg: ev.Reply})
		logMessage(b, &logEntry{origin: b, copies: msgs})
		return nil

	case chat.Delete:
		findMessage := makeFindMessage(b, event.origin, ev.ID)
		to := allChannelsExcept(b, event.origin)
		return deleteMessage(ctx, to, findMessage)

	case chat.Edit:
		findMessage := makeFindMessage(b, event.origin, ev.ID)
		to := allChannelsExcept(b, event.origin)
		return editMessage(ctx, to, findMessage, ev.Text)

	case chat.Join:
		msg := ev.Who.Name() + " joined " + origName
		to := allChannelsExcept(b, event.origin)
		_, err := sendMessage(ctx, to, nil, nil, msg)
		return err

	case chat.Leave:
		msg := ev.Who.Name() + " left " + origName
		to := allChannelsExcept(b, event.origin)
		_, err := sendMessage(ctx, to, nil, nil, msg)
		return err

	case chat.Rename:
		old := ev.From.Name()
		new := ev.To.Name()
		if old == new {
			break
		}
		msg := old + " renamed to " + new + " in " + origName
		to := allChannelsExcept(b, event.origin)
		_, err := sendMessage(ctx, to, nil, nil, msg)
		return err
	}
	return nil
}

// Receive returns the next event from any of the bridged channels.
func (b *Bridge) Receive(ctx context.Context) (interface{}, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ev, ok := <-b.recvOut:
		if !ok {
			return nil, io.EOF
		}
		return ev, nil
	}
}

func me(b *Bridge) chat.User {
	// TODO: use a more informative bridge User.
	// Option: get the User info from channels[0].
	return chat.User{
		ID:          chat.UserID("bridge"),
		Nick:        "bridge",
		FullName:    "bridge",
		DisplayName: "bridge",
	}
}

func nextID(b *Bridge) chat.MessageID {
	b.Lock()
	defer b.Unlock()
	b.nextID++
	return chat.MessageID(strconv.Itoa(b.nextID - 1))
}

func (b *Bridge) Send(ctx context.Context, text string) (chat.Message, error) {
	msgs, err := sendMessage(ctx, b.channels, nil, nil, text)
	if err != nil {
		return chat.Message{}, err
	}
	msg := chat.Message{ID: nextID(b), From: me(b), Text: text}
	msgs = append(msgs, message{to: b, msg: msg})
	logMessage(b, &logEntry{origin: b, copies: msgs})
	return msg, nil
}

func (b *Bridge) SendAs(ctx context.Context, sendAs chat.User, text string) (chat.Message, error) {
	msgs, err := sendMessage(ctx, b.channels, &sendAs, nil, text)
	if err != nil {
		return chat.Message{}, err
	}
	msg := chat.Message{ID: nextID(b), From: me(b), Text: text}
	msgs = append(msgs, message{to: b, msg: msg})
	logMessage(b, &logEntry{origin: b, copies: msgs})
	return msg, nil
}

func (b *Bridge) Reply(ctx context.Context, replyTo chat.Message, text string) (chat.Message, error) {
	findMessage := makeFindMessage(b, b, replyTo.ID)
	msgs, err := sendMessage(ctx, b.channels, nil, findMessage, text)
	if err != nil {
		return chat.Message{}, err
	}
	msg := chat.Message{ID: nextID(b), From: me(b), Text: text}
	msgs = append(msgs, message{to: b, msg: msg})
	logMessage(b, &logEntry{origin: b, copies: msgs})
	return msg, nil
}

func (b *Bridge) ReplyAs(ctx context.Context, sendAs chat.User, replyTo chat.Message, text string) (chat.Message, error) {
	findMessage := makeFindMessage(b, b, replyTo.ID)
	msgs, err := sendMessage(ctx, b.channels, &sendAs, findMessage, text)
	if err != nil {
		return chat.Message{}, err
	}
	msg := chat.Message{ID: nextID(b), From: me(b), Text: text}
	msgs = append(msgs, message{to: b, msg: msg})
	logMessage(b, &logEntry{origin: b, copies: msgs})
	return msg, nil
}

// Delete is a no-op for Bridge.
func (b *Bridge) Delete(context.Context, chat.MessageID) error { return nil }

// Edit is a no-op fro Bridge; it simply returns the given MessageID.
func (b *Bridge) Edit(_ context.Context, id chat.MessageID, _ string) (chat.MessageID, error) {
	return id, nil
}

// sendMessage sends a message to multiple channels,
// returning a slice of the messages.
func sendMessage(ctx context.Context,
	channels []chat.Channel,
	sendAs *chat.User,
	findMessage func(chat.Channel) *chat.Message,
	text string) ([]message, error) {

	if findMessage == nil {
		findMessage = func(chat.Channel) *chat.Message { return nil }
	}
	var group errgroup.Group
	messages := make([]message, len(channels))
	for i, ch := range channels {
		i, ch := i, ch
		group.Go(func() error {
			var err error
			var m chat.Message
			switch replyTo := findMessage(ch); {
			case replyTo != nil && sendAs == nil:
				m, err = ch.Reply(ctx, *replyTo, text)
			case replyTo != nil && sendAs != nil:
				m, err = ch.ReplyAs(ctx, *sendAs, *replyTo, text)
			case sendAs == nil:
				m, err = ch.Send(ctx, text)
			case sendAs != nil:
				m, err = ch.SendAs(ctx, *sendAs, text)
			}
			if err != nil {
				log.Printf("Failed to send message to %s on %s: %s\n",
					ch.Name(), ch.ServiceName(), err)
				return err
			}
			messages[i] = message{to: ch, msg: m}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return messages, nil
}

func editMessage(ctx context.Context, channels []chat.Channel, findMessage findMessageFunc, text string) error {
	var group errgroup.Group
	for _, ch := range channels {
		ch := ch
		group.Go(func() error {
			msg := findMessage(ch)
			if msg == nil {
				return nil
			}
			var err error
			if msg.ID, err = ch.Edit(ctx, msg.ID, text); err != nil {
				return fmt.Errorf("failed to send edit to %s on %s: %s",
					ch.Name(), ch.ServiceName(), err)
			}
			return nil
		})
	}
	return group.Wait()
}

func deleteMessage(ctx context.Context, channels []chat.Channel, findMessage findMessageFunc) error {
	var group errgroup.Group
	for _, ch := range channels {
		ch := ch
		group.Go(func() error {
			msg := findMessage(ch)
			if msg == nil {
				return nil
			}
			if err := ch.Delete(ctx, msg.ID); err != nil {
				return fmt.Errorf("failed to send delete to %s on %s: %s",
					ch.Name(), ch.ServiceName(), err)
			}
			return nil
		})
	}
	return group.Wait()
}

func allChannelsExcept(b *Bridge, exclude chat.Channel) []chat.Channel {
	var channels []chat.Channel
	for _, ch := range b.channels {
		if ch != exclude {
			channels = append(channels, ch)
		}
	}
	return channels
}

type findMessageFunc func(chat.Channel) *chat.Message

func makeFindMessage(b *Bridge, origin chat.Channel, id chat.MessageID) findMessageFunc {
	var entry *logEntry
outter:
	for _, e := range b.log {
		for _, c := range e.copies {
			if c.to == origin && c.msg.ID == id {
				entry = e
				break outter
			}
		}
	}
	return func(ch chat.Channel) *chat.Message {
		if entry == nil {
			return nil
		}
		for _, c := range entry.copies {
			if c.to == ch {
				return &c.msg
			}
		}
		return nil
	}
}
