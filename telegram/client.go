// Package telegram provides a Telegram bot client API.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/velour/chat"
)

const (
	minPhotoUpdateTime = 30 * time.Minute
	megabyte           = 1000000
	// Telegram's filesize limit for bots is 20 megabytes.
	fileSizeLimit = 20 * megabyte
)

var _ chat.Client = &Client{}

// Client implements the chat.Client interface using the Telegram bot API.
type Client struct {
	token string
	me    User
	error chan error
	close chan bool

	sync.Mutex
	channels map[int64]*channel
	users    map[int64]*user
	media    map[string]*media
	localURL *url.URL
}

type user struct {
	sync.Mutex
	User
	// photo is the file ID of the user's profile photo.
	photo string
	// photoTime is the last time the user's profile photo was updated.
	photoTime time.Time
}

type media struct {
	sync.Mutex
	File
	// Expires is the time that the URL expires.
	expires time.Time
}

// Dial returns a new Client using the given token.
func Dial(ctx context.Context, token string) (*Client, error) {
	c := &Client{
		token:    token,
		error:    make(chan error),
		close:    make(chan bool),
		channels: make(map[int64]*channel),
		users:    make(map[int64]*user),
		media:    make(map[string]*media),
	}
	if err := rpc(ctx, c, "getMe", nil, &c.me); err != nil {
		return nil, err
	}
	go poll(c)
	return c, nil
}

// Join returns a chat.Channel corresponding to
// a Telegram group, supergroup, chat, or channel ID.
// The ID string must be the base 10 chat ID number.
func (c *Client) Join(ctx context.Context, idString string) (chat.Channel, error) {
	var err error
	var req struct {
		ChatID int64 `json:"chat_id"`
	}
	if req.ChatID, err = strconv.ParseInt(idString, 10, 64); err != nil {
		return nil, err
	}
	var chat Chat
	if err := rpc(ctx, c, "getChat", req, &chat); err != nil {
		return nil, err
	}

	c.Lock()
	defer c.Unlock()
	var ch *channel
	if ch = c.channels[chat.ID]; ch == nil {
		ch = newChannel(c, chat)
		c.channels[chat.ID] = ch
	}
	return ch, nil
}

func (c *Client) Close(context.Context) error {
	close(c.close)
	err := <-c.error
	for _, ch := range c.channels {
		close(ch.in)
	}
	return err
}

// SetLocalURL enables URL generation for media, using the given URL as a prefix.
// For example, if SetLocalURL is called with "http://www.abc.com/telegram/media",
// all Channels on the Client will begin populating non-empty chat.User.PhotoURL fields
// of the form http://www.abc.com/telegram/media/<photo file>.
func (c *Client) SetLocalURL(u url.URL) {
	c.Lock()
	c.localURL = &u
	c.Unlock()
}

func poll(c *Client) {
	ctx := context.Background()
	req := struct {
		Offset  uint64 `json:"offset"`
		Timeout uint64 `json:"timeout"`
	}{}
	req.Timeout = 1 // second

	var err error
loop:
	for {
		var updates []Update
		if err = rpc(ctx, c, "getUpdates", req, &updates); err != nil {
			break
		}
		for _, u := range updates {
			if u.UpdateID < req.Offset {
				// The API actually does not state that the array of Updates is ordered.
				panic("out of order updates")
			}
			req.Offset = u.UpdateID + 1
			update(ctx, c, u)
		}
		select {
		case <-c.close:
			break loop
		default:
		}
	}
	c.error <- err
}

func update(ctx context.Context, c *Client, u Update) {
	var chat *Chat
	var from *User
	switch {
	case u.Message != nil:
		chat = &u.Message.Chat
		from = u.Message.From
	case u.EditedMessage != nil:
		chat = &u.EditedMessage.Chat
		from = u.EditedMessage.From
	}
	if chat == nil || chat.Title == nil {
		// Ignore messages not sent to supergroups, channels, or groups.
		return
	}

	c.Lock()
	defer c.Unlock()

	if from != nil {
		u, ok := c.users[from.ID]
		if !ok {
			u = &user{User: *from}
			c.users[from.ID] = u
		}
		go updateUser(ctx, c, u, *from)
	}

	var ch *channel
	if ch = c.channels[chat.ID]; ch == nil {
		ch = newChannel(c, *chat)
		c.channels[chat.ID] = ch
	}
	select {
	case ch.in <- []*Update{&u}:
	case us := <-ch.in:
		ch.in <- append(us, &u)
	}
}

func updateUser(ctx context.Context, c *Client, u *user, latest User) {
	u.Lock()
	defer u.Unlock()
	u.User = latest
	if time.Since(u.photoTime) < minPhotoUpdateTime {
		return
	}
	photo, err := getProfilePhoto(ctx, c, u.ID)
	if err != nil {
		log.Printf("Failed to get user %+v profile photo: %s\n", u, err)
		return
	}
	u.photo = photo
	u.photoTime = time.Now()
}

func getProfilePhoto(ctx context.Context, c *Client, userID int64) (string, error) {
	type userProfilePhotos struct {
		Photos [][]PhotoSize `json:"photos"`
	}
	var resp userProfilePhotos
	req := map[string]interface{}{"user_id": userID, "limit": 1}
	if err := rpc(ctx, c, "getUserProfilePhotos", req, &resp); err != nil {
		return "", err
	}
	if len(resp.Photos) == 0 {
		return "", nil
	}
	return biggestPhoto(resp.Photos[0]), nil
}

func biggestPhoto(photos []PhotoSize) string {
	var size int
	var photo string
	for _, ps := range photos {
		if ps.FileSize != nil && *ps.FileSize >= fileSizeLimit {
			continue
		}
		if sz := ps.Width * ps.Height; sz > size {
			photo = ps.FileID
			size = sz
		}
	}
	if photo == "" && len(photos) > 0 {
		return photos[0].FileID
	}
	return photo
}

// ServeHTTP serves files, photos, and other media from Telegram.
// It only handles GET requests, and
// the final path element of the request must be a Telegram File ID.
// The response is the corresponding file data.
func (c *Client) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if req.Method != http.MethodGet {
		http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		return
	}
	url, err := getMediaURL(ctx, c, path.Base(req.URL.Path))
	if err != nil {
		http.Error(w, "Telegram getFile failed", http.StatusBadRequest)
		return
	}
	if url == "" {
		http.Error(w, "Telegram file path missing", http.StatusBadRequest)
		return
	}
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if data, err := ioutil.ReadAll(io.LimitReader(resp.Body, 512)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		} else {
			http.Error(w, string(data), resp.StatusCode)
		}
		return
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func getMediaURL(ctx context.Context, c *Client, fileID string) (string, error) {
	c.Lock()
	m, ok := c.media[fileID]
	if !ok {
		m = new(media)
		c.media[fileID] = m
	}
	m.Lock()
	defer m.Unlock()
	c.Unlock()
	if !ok || time.Now().Before(m.expires) {
		var err error
		if m.File, err = getFile(ctx, c, fileID); err != nil {
			return "", err
		}
		// The URL is valid for an hour; expire it a bit before to be safe.
		m.expires = time.Now().Add(50 * time.Minute)
		c.media[fileID] = m
	}
	var url string
	if m.FilePath != nil {
		url = "https://api.telegram.org/file/bot" + c.token + "/" + *m.FilePath
	}
	return url, nil
}

func getFile(ctx context.Context, c *Client, fileID string) (File, error) {
	var resp File
	req := map[string]interface{}{"file_id": fileID}
	if err := rpc(ctx, c, "getFile", req, &resp); err != nil {
		return File{}, err
	}
	return resp, nil
}

func rpc(ctx context.Context, c *Client, method string, req interface{}, resp interface{}) error {
	err := make(chan error, 1)
	go func() { err <- _rpc(c, method, req, resp) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-err:
		return err
	}
}

func _rpc(c *Client, method string, req interface{}, resp interface{}) error {
	url := "https://api.telegram.org/bot" + c.token + "/" + method
	var err error
	var httpResp *http.Response
	if req == nil {
		httpResp, err = http.Get(url)
	} else {
		buf := bytes.NewBuffer(nil)
		if err = json.NewEncoder(buf).Encode(req); err != nil {
			return err
		}
		httpResp, err = http.Post(url, "application/json", buf)
	}
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	result := struct {
		OK          bool        `json:"ok"`
		Description *string     `json:"description"`
		Result      interface{} `json:"result"`
	}{}
	if resp != nil {
		result.Result = resp
	}
	switch err = json.NewDecoder(httpResp.Body).Decode(&result); {
	case !result.OK && result.Description != nil:
		return errors.New(*result.Description)
	case httpResp.StatusCode != http.StatusOK:
		return errors.New(httpResp.Status)
	case !result.OK:
		return errors.New("request failed")
	default:
		return nil
	}
}
