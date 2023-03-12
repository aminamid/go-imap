package imapclient

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// Options contains options for Client.
type Options struct {
	// Raw ingress and egress data will be written to this writer, if any
	DebugWriter io.Writer
	// Unilateral data handler
	UnilateralDataFunc UnilateralDataFunc
	// Decoder for RFC 2047 words
	WordDecoder *mime.WordDecoder
}

func (options *Options) wrapReadWriter(rw io.ReadWriter) io.ReadWriter {
	if options.DebugWriter == nil {
		return rw
	}
	return struct {
		io.Reader
		io.Writer
	}{
		Reader: io.TeeReader(rw, options.DebugWriter),
		Writer: io.MultiWriter(rw, options.DebugWriter),
	}
}

func (options *Options) decodeText(s string) (string, error) {
	wordDecoder := options.WordDecoder
	if wordDecoder == nil {
		wordDecoder = &mime.WordDecoder{}
	}
	out, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s, err
	}
	return out, nil
}

// Client is an IMAP client.
//
// IMAP commands are exposed as methods. These methods will block until the
// command has been sent to the server, but won't block until the server sends
// a response. They return a command struct which can be used to wait for the
// server response.
type Client struct {
	conn     net.Conn
	options  Options
	br       *bufio.Reader
	bw       *bufio.Writer
	dec      *imapwire.Decoder
	encMutex sync.Mutex

	mutex       sync.Mutex
	cmdTag      uint64
	pendingCmds []command
	contReqs    []continuationRequest
}

// New creates a new IMAP client.
//
// This function doesn't perform I/O.
//
// A nil options pointer is equivalent to a zero options value.
func New(conn net.Conn, options *Options) *Client {
	if options == nil {
		options = &Options{}
	}

	rw := options.wrapReadWriter(conn)
	br := bufio.NewReader(rw)
	bw := bufio.NewWriter(rw)

	client := &Client{
		conn:    conn,
		options: *options,
		br:      br,
		bw:      bw,
		dec:     imapwire.NewDecoder(br),
	}
	go client.read()
	return client
}

// DialTLS connects to an IMAP server with implicit TLS.
func DialTLS(address string, options *Options) (*Client, error) {
	conn, err := tls.Dial("tcp", address, nil)
	if err != nil {
		return nil, err
	}
	return New(conn, options), nil
}

// Close immediately closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// beginCommand starts sending a command to the server.
//
// The command name and a space are written.
//
// The caller must call commandEncoder.end.
func (c *Client) beginCommand(name string, cmd command) *commandEncoder {
	c.encMutex.Lock() // unlocked by commandEncoder.end

	c.mutex.Lock()
	c.cmdTag++
	tag := fmt.Sprintf("T%v", c.cmdTag)
	c.pendingCmds = append(c.pendingCmds, cmd)
	c.mutex.Unlock()

	baseCmd := cmd.base()
	*baseCmd = Command{
		tag:  tag,
		done: make(chan error, 1),
	}
	enc := &commandEncoder{
		Encoder: imapwire.NewEncoder(c.bw),
		client:  c,
		cmd:     baseCmd,
	}
	enc.Atom(tag).SP().Atom(name)
	return enc
}

func (c *Client) deletePendingCmdByTag(tag string) command {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for i, cmd := range c.pendingCmds {
		if cmd.base().tag == tag {
			c.pendingCmds = append(c.pendingCmds[:i], c.pendingCmds[i+1:]...)
			return cmd
		}
	}
	return nil
}

func findPendingCmdByType[T interface{}](c *Client) T {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, cmd := range c.pendingCmds {
		if cmd, ok := cmd.(T); ok {
			return cmd
		}
	}

	var cmd T
	return cmd
}

func (c *Client) registerContReq(cmd command) chan error {
	ch := make(chan error)

	c.mutex.Lock()
	c.contReqs = append(c.contReqs, continuationRequest{
		ch:  ch,
		cmd: cmd.base(),
	})
	c.mutex.Unlock()

	return ch
}

func (c *Client) unregisterContReq(ch chan error) {
	c.mutex.Lock()
	for i := range c.contReqs {
		if c.contReqs[i].ch == ch {
			c.contReqs = append(c.contReqs[:i], c.contReqs[i+1:]...)
			break
		}
	}
	c.mutex.Unlock()
}

// read continuously reads data coming from the server.
//
// All the data is decoded in the read goroutine, then dispatched via channels
// to pending commands.
func (c *Client) read() {
	defer func() {
		c.mutex.Lock()
		pendingCmds := c.pendingCmds
		c.pendingCmds = nil
		c.mutex.Unlock()

		for _, cmd := range pendingCmds {
			cmd.base().done <- io.ErrUnexpectedEOF
		}
	}()

	for {
		if c.dec.EOF() {
			break
		}
		if err := c.readResponse(); err != nil {
			// TODO: handle error
			log.Println(err)
			break
		}
	}
}

func (c *Client) readResponse() error {
	if c.dec.Special('+') {
		if err := c.readContinueReq(); err != nil {
			return fmt.Errorf("in continue-req: %v", err)
		}
		return nil
	}

	var tag, typ string
	if !c.dec.Expect(c.dec.Special('*') || c.dec.Atom(&tag), "'*' or atom") {
		return fmt.Errorf("in response: cannot read tag: %v", c.dec.Err())
	}
	if !c.dec.ExpectSP() {
		return fmt.Errorf("in response: %v", c.dec.Err())
	}
	if !c.dec.ExpectAtom(&typ) {
		return fmt.Errorf("in response: cannot read type: %v", c.dec.Err())
	}

	var (
		token    string
		err      error
		startTLS *startTLSCommand
	)
	if tag != "" {
		token = "response-tagged"
		startTLS, err = c.readResponseTagged(tag, typ)
	} else if typ == "BYE" {
		token = "resp-cond-bye"
		var text string
		if !c.dec.ExpectText(&text) {
			return fmt.Errorf("in resp-text: %v", c.dec.Err())
		}
	} else {
		token = "response-data"
		err = c.readResponseData(typ)
	}
	if err != nil {
		return fmt.Errorf("in %v: %v", token, err)
	}

	if !c.dec.ExpectCRLF() {
		return fmt.Errorf("in response: %v", c.dec.Err())
	}

	if startTLS != nil {
		c.upgradeStartTLS(startTLS.tlsConfig)
		close(startTLS.upgradeDone)
	}

	return nil
}

func (c *Client) readContinueReq() error {
	var text string
	if !c.dec.ExpectSP() || !c.dec.ExpectText(&text) || !c.dec.ExpectCRLF() {
		return c.dec.Err()
	}

	var ch chan<- error
	c.mutex.Lock()
	if len(c.contReqs) > 0 {
		ch = c.contReqs[0].ch
		c.contReqs = append(c.contReqs[:0], c.contReqs[1:]...)
	}
	c.mutex.Unlock()

	if ch == nil {
		return fmt.Errorf("received unmatched continuation request")
	}

	ch <- nil
	close(ch)
	return nil
}

func (c *Client) readResponseTagged(tag, typ string) (*startTLSCommand, error) {
	if !c.dec.ExpectSP() {
		return nil, c.dec.Err()
	}
	if c.dec.Special('[') { // resp-text-code
		var code string
		if !c.dec.ExpectAtom(&code) {
			return nil, fmt.Errorf("in resp-text-code: %v", c.dec.Err())
		}
		switch code {
		case "CAPABILITY": // capability-data
			if _, err := readCapabilities(c.dec); err != nil {
				return nil, err
			}
		default: // [SP 1*<any TEXT-CHAR except "]">]
			if c.dec.SP() {
				c.dec.Skip(']')
			}
		}
		if !c.dec.ExpectSpecial(']') || !c.dec.ExpectSP() {
			return nil, fmt.Errorf("in resp-text: %v", c.dec.Err())
		}
	}
	var text string
	if !c.dec.ExpectText(&text) {
		return nil, fmt.Errorf("in resp-text: %v", c.dec.Err())
	}

	var cmdErr error
	switch typ {
	case "OK":
		// nothing to do
	case "NO", "BAD":
		// TODO: define a type for IMAP errors
		cmdErr = fmt.Errorf("%v %v", typ, text)
	default:
		return nil, fmt.Errorf("in resp-cond-state: expected OK, NO or BAD status condition, but got %v", typ)
	}

	cmd := c.deletePendingCmdByTag(tag)
	if cmd == nil {
		return nil, fmt.Errorf("received tagged response with unknown tag %q", tag)
	}

	done := cmd.base().done
	done <- cmdErr
	close(done)

	// Ensure the command is not blocked waiting on continuation requests
	c.mutex.Lock()
	var filtered []continuationRequest
	for _, contReq := range c.contReqs {
		if contReq.cmd != cmd.base() {
			filtered = append(filtered, contReq)
		} else {
			if cmdErr != nil {
				contReq.ch <- cmdErr
			}
			close(contReq.ch)
		}
	}
	c.contReqs = filtered
	c.mutex.Unlock()

	var startTLS *startTLSCommand
	switch cmd := cmd.(type) {
	case *startTLSCommand:
		if cmdErr == nil {
			startTLS = cmd
		}
	case *ListCommand:
		close(cmd.mailboxes)
	case *FetchCommand:
		close(cmd.msgs)
	}

	return startTLS, nil
}

func (c *Client) readResponseData(typ string) error {
	// number SP "EXISTS" / number SP "RECENT" / number SP "FETCH"
	var num uint32
	if typ[0] >= '0' && typ[0] <= '9' {
		v, err := strconv.ParseUint(typ, 10, 32)
		if err != nil {
			return err
		}

		num = uint32(v)
		if !c.dec.ExpectSP() || !c.dec.ExpectAtom(&typ) {
			return c.dec.Err()
		}
	}

	var unilateralData UnilateralData
	switch typ {
	case "OK", "NO", "BAD": // resp-cond-state
		// TODO
		var text string
		if !c.dec.ExpectText(&text) {
			return fmt.Errorf("in resp-text: %v", c.dec.Err())
		}
	case "CAPABILITY": // capability-data
		caps, err := readCapabilities(c.dec)
		if err != nil {
			return err
		}
		if cmd := findPendingCmdByType[*CapabilityCommand](c); cmd != nil {
			cmd.caps = caps
		}
	case "FLAGS":
		if !c.dec.ExpectSP() {
			return c.dec.Err()
		}
		flags, err := readFlagList(c.dec)
		if err != nil {
			return err
		}
		cmd := findPendingCmdByType[*SelectCommand](c)
		if cmd != nil {
			cmd.data.Flags = flags
		}
	case "EXISTS":
		cmd := findPendingCmdByType[*SelectCommand](c)
		if cmd != nil {
			cmd.data.NumMessages = num
		} else {
			unilateralData = &UnilateralDataMailbox{NumMessages: &num}
		}
	case "RECENT":
		// ignore
	case "LIST":
		if !c.dec.ExpectSP() {
			return c.dec.Err()
		}
		data, err := readList(c.dec)
		if err != nil {
			return fmt.Errorf("in LIST: %v", err)
		}
		if cmd := findPendingCmdByType[*ListCommand](c); cmd != nil {
			cmd.mailboxes <- data
		}
	case "STATUS":
		if !c.dec.ExpectSP() {
			return c.dec.Err()
		}
		cmd := findPendingCmdByType[*StatusCommand](c)
		if err := readStatus(c.dec, cmd); err != nil {
			return fmt.Errorf("in status: %v", err)
		}
	case "FETCH":
		if !c.dec.ExpectSP() {
			return c.dec.Err()
		}
		cmd := findPendingCmdByType[*FetchCommand](c)
		if err := readMsgAtt(c.dec, num, cmd, &c.options); err != nil {
			return fmt.Errorf("in msg-att: %v", err)
		}
	default:
		return fmt.Errorf("unsupported response type %q", typ)
	}

	if unilateralData != nil && c.options.UnilateralDataFunc != nil {
		c.options.UnilateralDataFunc(unilateralData)
	}

	return nil
}

// Noop sends a NOOP command.
func (c *Client) Noop() *Command {
	cmd := &Command{}
	c.beginCommand("NOOP", cmd).end()
	return cmd
}

// Logout sends a LOGOUT command.
//
// This command informs the server that the client is done with the connection.
func (c *Client) Logout() *LogoutCommand {
	cmd := &LogoutCommand{closer: c}
	c.beginCommand("LOGOUT", cmd).end()
	return cmd
}

// Capability sends a CAPABILITY command.
func (c *Client) Capability() *CapabilityCommand {
	cmd := &CapabilityCommand{}
	c.beginCommand("CAPABILITY", cmd).end()
	return cmd
}

// Login sends a LOGIN command.
func (c *Client) Login(username, password string) *Command {
	cmd := &Command{}
	enc := c.beginCommand("LOGIN", cmd)
	enc.SP().String(username).SP().String(password)
	enc.end()
	return cmd
}

// StartTLS sends a STARTTLS command.
//
// Unlike other commands, this method blocks until the command completes.
func (c *Client) StartTLS(config *tls.Config) error {
	upgradeDone := make(chan struct{})
	cmd := &startTLSCommand{
		tlsConfig:   config,
		upgradeDone: upgradeDone,
	}
	enc := c.beginCommand("STARTTLS", cmd)
	enc.flush()
	defer enc.end()

	// Once a client issues a STARTTLS command, it MUST NOT issue further
	// commands until a server response is seen and the TLS negotiation is
	// complete

	if err := cmd.Wait(); err != nil {
		return err
	}

	// The decoder goroutine will invoke Client.upgradeStartTLS
	<-upgradeDone
	return nil
}

func (c *Client) upgradeStartTLS(tlsConfig *tls.Config) {
	// Drain buffered data from our bufio.Reader
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, c.br, int64(c.br.Buffered())); err != nil {
		panic(err) // unreachable
	}

	var cleartextConn net.Conn
	if buf.Len() > 0 {
		r := io.MultiReader(&buf, c.conn)
		cleartextConn = startTLSConn{c.conn, r}
	} else {
		cleartextConn = c.conn
	}

	tlsConn := tls.Client(cleartextConn, tlsConfig)
	rw := c.options.wrapReadWriter(tlsConn)

	c.br.Reset(rw)
	// Unfortunately we can't re-use the bufio.Writer here, it races with
	// Client.StartTLS
	c.bw = bufio.NewWriter(rw)
}

// Append sends an APPEND command.
//
// The caller must call AppendCommand.Close.
func (c *Client) Append(mailbox string, size int64) *AppendCommand {
	// TODO: flag parenthesized list, date/time string
	cmd := &AppendCommand{}
	cmd.enc = c.beginCommand("APPEND", cmd)
	cmd.enc.SP().Mailbox(mailbox).SP()
	cmd.wc = cmd.enc.Literal(size)
	return cmd
}

// Select sends a SELECT command.
func (c *Client) Select(mailbox string) *SelectCommand {
	cmd := &SelectCommand{}
	enc := c.beginCommand("SELECT", cmd)
	enc.SP().Mailbox(mailbox)
	enc.end()
	return cmd
}

// List sends a LIST command.
//
// The caller must fully consume the ListCommand. A simple way to do so is to
// defer a call to ListCommand.Close.
func (c *Client) List(ref, pattern string) *ListCommand {
	// TODO: multiple patterns
	// TODO: extended variant
	cmd := &ListCommand{mailboxes: make(chan *ListData, 64)}
	enc := c.beginCommand("LIST", cmd)
	// TODO: second arg is mbox-or-pat
	enc.SP().Mailbox(ref).SP().Atom(pattern)
	enc.end()
	return cmd
}

// Status sends a STATUS command.
func (c *Client) Status(mailbox string, items []StatusItem) *StatusCommand {
	cmd := &StatusCommand{}
	enc := c.beginCommand("STATUS", cmd)
	enc.SP().Mailbox(mailbox).SP()
	enc.List(len(items), func(i int) {
		enc.Atom(string(items[i]))
	})
	enc.end()
	return cmd
}

// Fetch sends a FETCH command.
//
// The caller must fully consume the FetchCommand. A simple way to do so is to
// defer a call to FetchCommand.Close.
func (c *Client) Fetch(seqSet imap.SeqSet, items []FetchItem) *FetchCommand {
	// TODO: sequence set, message data item names or macro
	cmd := &FetchCommand{
		msgs: make(chan *FetchMessageData, 128),
	}
	enc := c.beginCommand("FETCH", cmd)
	enc.SP().Atom(seqSet.String()).SP().List(len(items), func(i int) {
		writeFetchItem(enc.Encoder, items[i])
	})
	enc.end()
	return cmd
}

// Idle sends an IDLE command.
//
// Unlike other commands, this method blocks until the server acknowledges it.
// On success, the IDLE command is running and other commands cannot be sent.
// The caller must invoke IdleCommand.Close to stop IDLE and unblock the
// client.
func (c *Client) Idle() (*IdleCommand, error) {
	cmd := &IdleCommand{}
	contReq := c.registerContReq(cmd)
	cmd.enc = c.beginCommand("IDLE", cmd)
	cmd.enc.flush()

	select {
	case <-contReq:
		return cmd, nil
	case err := <-cmd.done:
		c.unregisterContReq(contReq)
		cmd.enc.end()
		return nil, err
	}
}

type commandEncoder struct {
	*imapwire.Encoder
	client *Client
	cmd    *Command
}

// end ends an outgoing command.
//
// A CRLF is written, the encoder is flushed and its lock is released.
func (ce *commandEncoder) end() {
	if ce.Encoder != nil {
		ce.flush()
	}
	ce.client.encMutex.Unlock()
}

// flush sends an outgoing command, but keeps the encoder lock.
//
// A CRLF is written and the encoder is flushed. Callers must call
// commandEncoder.end to release the lock.
func (ce *commandEncoder) flush() {
	if err := ce.Encoder.CRLF(); err != nil {
		ce.cmd.err = err
	}
	ce.Encoder = nil
}

// Literal encodes a literal.
func (ce *commandEncoder) Literal(size int64) io.WriteCloser {
	contReq := ce.client.registerContReq(ce.cmd)
	return ce.Encoder.Literal(size, contReq)
}

// continuationRequest is a pending continuation request.
type continuationRequest struct {
	ch  chan error
	cmd *Command
}

// UnilateralData holds unilateral data.
//
// Unilateral data is data that the client didn't explicitly request.
type UnilateralData interface {
	unilateralData()
}

var (
	_ UnilateralData = (*UnilateralDataMailbox)(nil)
)

// UnilateralDataMailbox describes a mailbox status update.
//
// If a field is nil, it hasn't changed.
type UnilateralDataMailbox struct {
	NumMessages *uint32
}

func (*UnilateralDataMailbox) unilateralData() {}

// UnilateralDataFunc handles unilateral data.
//
// The handler will block the client while running. If the caller intends to
// perform slow operations, a buffered channel and a separate goroutine should
// be used.
//
// The handler will be invoked in an arbitrary goroutine.
//
// See Options.UnilateralDataFunc.
type UnilateralDataFunc func(data UnilateralData)

// command is an interface for IMAP commands.
//
// Commands are represented by the Command type, but can be extended by other
// types (e.g. CapabilityCommand).
type command interface {
	base() *Command
}

// Command is a basic IMAP command.
type Command struct {
	tag  string
	done chan error
	err  error
}

func (cmd *Command) base() *Command {
	return cmd
}

// Wait blocks until the command has completed.
func (cmd *Command) Wait() error {
	if cmd.err == nil {
		cmd.err = <-cmd.done
	}
	return cmd.err
}

type cmd = Command // type alias to avoid exporting anonymous struct fields

// CapabilityCommand is a CAPABILITY command.
type CapabilityCommand struct {
	cmd
	caps map[string]struct{}
}

func (cmd *CapabilityCommand) Wait() (map[string]struct{}, error) {
	err := cmd.cmd.Wait()
	return cmd.caps, err
}

// LogoutCommand is a LOGOUT command.
type LogoutCommand struct {
	cmd
	closer io.Closer
}

func (cmd *LogoutCommand) Wait() error {
	if err := cmd.cmd.Wait(); err != nil {
		return err
	}
	return cmd.closer.Close()
}

// AppendCommand is an APPEND command.
//
// Callers must write the message contents, then call Close.
type AppendCommand struct {
	cmd
	enc *commandEncoder
	wc  io.WriteCloser
}

func (cmd *AppendCommand) Write(b []byte) (int, error) {
	return cmd.wc.Write(b)
}

func (cmd *AppendCommand) Close() error {
	err := cmd.wc.Close()
	if cmd.enc != nil {
		cmd.enc.end()
		cmd.enc = nil
	}
	return err
}

func (cmd *AppendCommand) Wait() error {
	return cmd.cmd.Wait()
}

// SelectCommand is a SELECT command.
type SelectCommand struct {
	cmd
	data SelectData
}

func (cmd *SelectCommand) Wait() (*SelectData, error) {
	return &cmd.data, cmd.cmd.Wait()
}

// SelectData is the data returned by a SELECT command.
type SelectData struct {
	// Flags defined for this mailbox
	Flags []imap.Flag
	// Number of messages in this mailbox (aka. "EXISTS")
	NumMessages uint32

	// TODO: LIST, PERMANENTFLAGS, UIDNEXT, UIDVALIDITY
}

type startTLSCommand struct {
	cmd
	tlsConfig   *tls.Config
	upgradeDone chan<- struct{}
}

// ListCommand is a LIST command.
type ListCommand struct {
	cmd
	mailboxes chan *ListData
}

// Next advances to the next mailbox.
//
// On success, the mailbox LIST data is returned. On error or if there are no
// more mailboxes, nil is returned.
func (cmd *ListCommand) Next() *ListData {
	return <-cmd.mailboxes
}

// Close releases the command.
//
// Calling Close unblocks the IMAP client decoder and lets it read the next
// responses. Next will always return nil after Close.
func (cmd *ListCommand) Close() error {
	for cmd.Next() != nil {
		// ignore
	}
	return cmd.cmd.Wait()
}

// Collect accumulates mailboxes into a list.
//
// This is equivalent to calling Next repeatedly and then Close.
func (cmd *ListCommand) Collect() ([]*ListData, error) {
	var l []*ListData
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		l = append(l, data)
	}
	return l, cmd.Close()
}

// ListData is the mailbox data returned by a LIST command.
type ListData struct {
	Attrs   []imap.MailboxAttr
	Delim   rune
	Mailbox string
}

// StatusCommand is a STATUS command.
type StatusCommand struct {
	cmd
	data StatusData
}

func (cmd *StatusCommand) Wait() (*StatusData, error) {
	return &cmd.data, cmd.cmd.Wait()
}

// StatusItem is a data item which can be requested by a STATUS command.
type StatusItem string

const (
	StatusItemNumMessages StatusItem = "MESSAGES"
	StatusItemUIDNext     StatusItem = "UIDNEXT"
	StatusItemUIDValidity StatusItem = "UIDVALIDITY"
	StatusItemNumUnseen   StatusItem = "UNSEEN"
	StatusItemNumDeleted  StatusItem = "DELETED"
	StatusItemSize        StatusItem = "SIZE"
)

// StatusData is the data returned by a STATUS command.
//
// The mailbox name is always populated. The remaining fields are optional.
type StatusData struct {
	Mailbox string

	NumMessages *uint32
	UIDNext     uint32
	UIDValidity uint32
	NumUnseen   *uint32
	NumDeleted  *uint32
	Size        *int64
}

// FetchItem is a message data item which can be requested by a FETCH command.
type FetchItem interface {
	fetchItem()
}

var (
	_ FetchItem = FetchItemKeyword("")
	_ FetchItem = (*FetchItemBodySection)(nil)
	_ FetchItem = (*FetchItemBinarySection)(nil)
	_ FetchItem = (*FetchItemBinarySectionSize)(nil)
)

// FetchItemKeyword is a FETCH item described by a single keyword.
type FetchItemKeyword string

func (FetchItemKeyword) fetchItem() {}

var (
	// Macros
	FetchItemAll  FetchItem = FetchItemKeyword("ALL")
	FetchItemFast FetchItem = FetchItemKeyword("FAST")
	FetchItemFull FetchItem = FetchItemKeyword("FULL")

	FetchItemBody          FetchItem = FetchItemKeyword("BODY")
	FetchItemBodyStructure FetchItem = FetchItemKeyword("BODYSTRUCTURE")
	FetchItemEnvelope      FetchItem = FetchItemKeyword("ENVELOPE")
	FetchItemFlags         FetchItem = FetchItemKeyword("FLAGS")
	FetchItemInternalDate  FetchItem = FetchItemKeyword("INTERNALDATE")
	FetchItemRFC822Size    FetchItem = FetchItemKeyword("RFC822.SIZE")
	FetchItemUID           FetchItem = FetchItemKeyword("UID")
)

type PartSpecifier string

const (
	PartSpecifierNone   PartSpecifier = ""
	PartSpecifierHeader PartSpecifier = "HEADER"
	PartSpecifierMIME   PartSpecifier = "MIME"
	PartSpecifierText   PartSpecifier = "TEXT"
)

type SectionPartial struct {
	Offset, Size int64
}

// FetchItemBodySection is a FETCH BODY[] data item.
type FetchItemBodySection struct {
	Specifier       PartSpecifier
	Part            []int
	HeaderFields    []string
	HeaderFieldsNot []string
	Partial         *SectionPartial
	Peek            bool
}

func (*FetchItemBodySection) fetchItem() {}

// FetchItemBinarySection is a FETCH BINARY[] data item.
type FetchItemBinarySection struct {
	Part    []int
	Partial *SectionPartial
	Peek    bool
}

func (*FetchItemBinarySection) fetchItem() {}

// FetchItemBinarySectionSize is a FETCH BINARY.SIZE[] data item.
type FetchItemBinarySectionSize struct {
	Part []int
}

func (*FetchItemBinarySectionSize) fetchItem() {}

func writeFetchItem(enc *imapwire.Encoder, item FetchItem) {
	switch item := item.(type) {
	case FetchItemKeyword:
		enc.Atom(string(item))
	case *FetchItemBodySection:
		enc.Atom("BODY")
		if item.Peek {
			enc.Atom(".PEEK")
		}
		enc.Special('[')
		writeSectionPart(enc, item.Part)
		if len(item.Part) > 0 && item.Specifier != PartSpecifierNone {
			enc.Special('.')
		}
		if item.Specifier != PartSpecifierNone {
			enc.Atom(string(item.Specifier))

			var headerList []string
			if len(item.HeaderFields) > 0 {
				headerList = item.HeaderFields
				enc.Atom(".FIELDS")
			} else if len(item.HeaderFieldsNot) > 0 {
				headerList = item.HeaderFieldsNot
				enc.Atom(".FIELDS.NOT")
			}

			if len(headerList) > 0 {
				enc.SP().List(len(headerList), func(i int) {
					enc.String(headerList[i])
				})
			}
		}
		enc.Special(']')
		writeSectionPartial(enc, item.Partial)
	case *FetchItemBinarySection:
		enc.Atom("BINARY")
		if item.Peek {
			enc.Atom(".PEEK")
		}
		enc.Special('[')
		writeSectionPart(enc, item.Part)
		enc.Special(']')
		writeSectionPartial(enc, item.Partial)
	case *FetchItemBinarySectionSize:
		enc.Atom("BINARY.SIZE")
		enc.Special('[')
		writeSectionPart(enc, item.Part)
		enc.Special(']')
	default:
		panic(fmt.Errorf("imapclient: unknown fetch item type %T", item))
	}
}

func writeSectionPart(enc *imapwire.Encoder, part []int) {
	if len(part) == 0 {
		return
	}

	var l []string
	for _, num := range part {
		l = append(l, fmt.Sprintf("%v", num))
	}
	enc.Atom(strings.Join(l, "."))
}

func writeSectionPartial(enc *imapwire.Encoder, partial *SectionPartial) {
	if partial == nil {
		return
	}
	enc.Special('<').Number64(partial.Offset).Special('.').Number64(partial.Size).Special('>')
}

// FetchCommand is a FETCH command.
type FetchCommand struct {
	cmd
	msgs chan *FetchMessageData
	prev *FetchMessageData
}

// Next advances to the next message.
//
// On success, the message is returned. On error or if there are no more
// messages, nil is returned. To check the error value, use Close.
func (cmd *FetchCommand) Next() *FetchMessageData {
	if cmd.prev != nil {
		cmd.prev.discard()
	}
	return <-cmd.msgs
}

// Close releases the command.
//
// Calling Close unblocks the IMAP client decoder and lets it read the next
// responses. Next will always return nil after Close.
func (cmd *FetchCommand) Close() error {
	for cmd.Next() != nil {
		// ignore
	}
	return cmd.cmd.Wait()
}

// Collect accumulates message data into a list.
//
// This method will read and store message contents in memory. This is
// acceptable when the message contents have a reasonable size, but may not be
// suitable when fetching e.g. attachments.
//
// This is equivalent to calling Next repeatedly and then Close.
func (cmd *FetchCommand) Collect() ([]*FetchMessageBuffer, error) {
	defer cmd.Close()

	var l []*FetchMessageBuffer
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}

		buf, err := msg.Collect()
		if err != nil {
			return l, err
		}

		l = append(l, buf)
	}
	return l, cmd.Close()
}

// FetchMessageData contains a message's FETCH data.
type FetchMessageData struct {
	SeqNum uint32

	items chan FetchItemData
	prev  FetchItemData
}

// Next advances to the next data item for this message.
//
// If there is one or more data items left, the next item is returned.
// Otherwise nil is returned.
func (data *FetchMessageData) Next() FetchItemData {
	if d, ok := data.prev.(discarder); ok {
		d.discard()
	}

	item := <-data.items
	data.prev = item
	return item
}

func (data *FetchMessageData) discard() {
	for {
		if item := data.Next(); item == nil {
			break
		}
	}
}

// Collect accumulates message data into a struct.
//
// This method will read and store message contents in memory. This is
// acceptable when the message contents have a reasonable size, but may not be
// suitable when fetching e.g. attachments.
func (data *FetchMessageData) Collect() (*FetchMessageBuffer, error) {
	defer data.discard()

	buf := &FetchMessageBuffer{SeqNum: data.SeqNum}
	for {
		item := data.Next()
		if item == nil {
			break
		}
		if err := buf.populateItemData(item); err != nil {
			return buf, err
		}
	}
	return buf, nil
}

// FetchItemData contains a message's FETCH item data.
type FetchItemData interface {
	FetchItem() FetchItem
	fetchItemData()
}

var (
	_ FetchItemData = FetchItemDataSection{}
	_ FetchItemData = FetchItemDataFlags{}
	_ FetchItemData = FetchItemDataEnvelope{}
	_ FetchItemData = FetchItemDataInternalDate{}
	_ FetchItemData = FetchItemDataRFC822Size{}
	_ FetchItemData = FetchItemDataUID{}
	_ FetchItemData = FetchItemDataBodyStructure{}
)

type discarder interface {
	discard()
}

var _ discarder = FetchItemDataSection{}

// FetchItemDataSection holds data returned by FETCH BODY[] and BINARY[].
type FetchItemDataSection struct {
	Section FetchItem // either *FetchItemBodySection or *FetchItemBinarySection
	Literal LiteralReader
}

func (item FetchItemDataSection) FetchItem() FetchItem {
	return item.Section
}

func (FetchItemDataSection) fetchItemData() {}

func (item FetchItemDataSection) discard() {
	io.Copy(io.Discard, item.Literal)
}

// FetchItemDataFlags holds data returned by FETCH FLAGS.
type FetchItemDataFlags struct {
	Flags []imap.Flag
}

func (FetchItemDataFlags) FetchItem() FetchItem {
	return FetchItemFlags
}

func (FetchItemDataFlags) fetchItemData() {}

// FetchItemDataEnvelope holds data returned by FETCH ENVELOPE.
type FetchItemDataEnvelope struct {
	Envelope *Envelope
}

func (FetchItemDataEnvelope) FetchItem() FetchItem {
	return FetchItemEnvelope
}

func (FetchItemDataEnvelope) fetchItemData() {}

// FetchItemDataInternalDate holds data returned by FETCH INTERNALDATE.
type FetchItemDataInternalDate struct {
	Time time.Time
}

func (FetchItemDataInternalDate) FetchItem() FetchItem {
	return FetchItemInternalDate
}

func (FetchItemDataInternalDate) fetchItemData() {}

// FetchItemDataRFC822Size holds data returned by FETCH RFC822.SIZE.
type FetchItemDataRFC822Size struct {
	Size int64
}

func (FetchItemDataRFC822Size) FetchItem() FetchItem {
	return FetchItemRFC822Size
}

func (FetchItemDataRFC822Size) fetchItemData() {}

// FetchItemDataUID holds data returned by FETCH UID.
type FetchItemDataUID struct {
	UID uint32
}

func (FetchItemDataUID) FetchItem() FetchItem {
	return FetchItemUID
}

func (FetchItemDataUID) fetchItemData() {}

// FetchItemDataBodyStructure holds data returned by FETCH BODYSTRUCTURE or
// FETCH BODY.
type FetchItemDataBodyStructure struct {
	BodyStructure BodyStructure
	IsExtended    bool // True if BODYSTRUCTURE, false if BODY
}

func (item FetchItemDataBodyStructure) FetchItem() FetchItem {
	if item.IsExtended {
		return FetchItemBodyStructure
	} else {
		return FetchItemBody
	}
}

func (FetchItemDataBodyStructure) fetchItemData() {}

// FetchItemDataBinarySectionSize holds data returned by FETCH BINARY.SIZE[].
type FetchItemDataBinarySectionSize struct {
	Part []int
	Size uint32
}

func (item FetchItemDataBinarySectionSize) FetchItem() FetchItem {
	return &FetchItemBinarySectionSize{Part: item.Part}
}

func (FetchItemDataBinarySectionSize) fetchItemData() {}

// Envelope is the envelope structure of a message.
type Envelope struct {
	Date      string // see net/mail.ParseDate
	Subject   string
	From      []Address
	Sender    []Address
	ReplyTo   []Address
	To        []Address
	Cc        []Address
	Bcc       []Address
	InReplyTo string
	MessageID string
}

// Address represents a sender or recipient of a message.
type Address struct {
	Name    string
	Mailbox string
	Host    string
}

// Addr returns the e-mail address in the form "foo@example.org".
//
// If the address is a start or end of group, the empty string is returned.
func (addr *Address) Addr() string {
	if addr.Mailbox == "" || addr.Host == "" {
		return ""
	}
	return addr.Mailbox + "@" + addr.Host
}

// IsGroupStart returns true if this address is a start of group marker.
//
// In that case, Mailbox contains the group name phrase.
func (addr *Address) IsGroupStart() bool {
	return addr.Host == "" && addr.Mailbox != ""
}

// IsGroupEnd returns true if this address is a end of group marker.
func (addr *Address) IsGroupEnd() bool {
	return addr.Host == "" && addr.Mailbox == ""
}

// BodyStructure describes the body structure of a message.
//
// A BodyStructure value is either a *BodyStructureSinglePart or a
// *BodyStructureMultiPart.
type BodyStructure interface {
	// MediaType returns the MIME type of this body structure, e.g. "text/plain".
	MediaType() string
	// Walk walks the body structure tree, calling f for each part in the tree,
	// including bs itself. The parts are visited in DFS pre-order.
	Walk(f BodyStructureWalkFunc)
	// Disposition returns the body structure disposition, if available.
	Disposition() *BodyStructureDisposition

	bodyStructure()
}

var (
	_ BodyStructure = (*BodyStructureSinglePart)(nil)
	_ BodyStructure = (*BodyStructureMultiPart)(nil)
)

// BodyStructureSinglePart is a body structure with a single part.
type BodyStructureSinglePart struct {
	Type, Subtype string
	Params        map[string]string
	ID            string
	Description   string
	Encoding      string
	Size          uint32

	MessageRFC822 *BodyStructureMessageRFC822 // only for "message/rfc822"
	Text          *BodyStructureText          // only for "text/*"
	Extended      *BodyStructureSinglePartExt
}

func (bs *BodyStructureSinglePart) MediaType() string {
	return strings.ToLower(bs.Type) + "/" + strings.ToLower(bs.Subtype)
}

func (bs *BodyStructureSinglePart) Walk(f BodyStructureWalkFunc) {
	f([]int{1}, bs)
}

func (bs *BodyStructureSinglePart) Disposition() *BodyStructureDisposition {
	if bs.Extended == nil {
		return nil
	}
	return bs.Extended.Disposition
}

// Filename decodes the body structure's filename, if any.
func (bs *BodyStructureSinglePart) Filename() string {
	var filename string
	if bs.Extended != nil && bs.Extended.Disposition != nil {
		filename = bs.Extended.Disposition.Params["filename"]
	}
	if filename == "" {
		// Note: using "name" in Content-Type is discouraged
		filename = bs.Params["name"]
	}
	return filename
}

func (*BodyStructureSinglePart) bodyStructure() {}

type BodyStructureMessageRFC822 struct {
	Envelope      *Envelope
	BodyStructure BodyStructure
	NumLines      int64
}

type BodyStructureText struct {
	NumLines int64
}

type BodyStructureSinglePartExt struct {
	MD5         string
	Disposition *BodyStructureDisposition
	Language    []string
	Location    string
}

// BodyStructureMultiPart is a body structure with multiple parts.
type BodyStructureMultiPart struct {
	Children []BodyStructure
	Subtype  string

	Extended *BodyStructureMultiPartExt
}

func (bs *BodyStructureMultiPart) MediaType() string {
	return "multipart/" + strings.ToLower(bs.Subtype)
}

func (bs *BodyStructureMultiPart) Walk(f BodyStructureWalkFunc) {
	bs.walk(f, nil)
}

func (bs *BodyStructureMultiPart) walk(f BodyStructureWalkFunc, path []int) {
	if !f(path, bs) {
		return
	}

	pathBuf := make([]int, len(path))
	copy(pathBuf, path)
	for i, part := range bs.Children {
		num := i + 1
		partPath := append(pathBuf, num)

		switch part := part.(type) {
		case *BodyStructureSinglePart:
			f(partPath, part)
		case *BodyStructureMultiPart:
			part.walk(f, partPath)
		default:
			panic(fmt.Errorf("unsupported body structure type %T", part))
		}
	}
}

func (bs *BodyStructureMultiPart) Disposition() *BodyStructureDisposition {
	if bs.Extended == nil {
		return nil
	}
	return bs.Extended.Disposition
}

func (*BodyStructureMultiPart) bodyStructure() {}

type BodyStructureMultiPartExt struct {
	Params      map[string]string
	Disposition *BodyStructureDisposition
	Language    []string
	Location    string
}

type BodyStructureDisposition struct {
	Value  string
	Params map[string]string
}

// BodyStructureWalkFunc is a function called for each body structure visited
// by BodyStructure.Walk.
//
// The path argument contains the IMAP part path.
//
// The function should return true to visit all of the part's children or false
// to skip them.
type BodyStructureWalkFunc func(path []int, part BodyStructure) (walkChildren bool)

// LiteralReader is a reader for IMAP literals.
type LiteralReader interface {
	io.Reader
	Size() int64
}

// FetchMessageBuffer is a buffer for the data returned by FetchMessageData.
//
// The SeqNum field is always populated. All remaining fields are optional.
type FetchMessageBuffer struct {
	SeqNum        uint32
	Flags         []imap.Flag
	Envelope      *Envelope
	InternalDate  time.Time
	RFC822Size    int64
	UID           uint32
	BodyStructure BodyStructure
	Section       map[FetchItem][]byte
}

func (buf *FetchMessageBuffer) populateItemData(item FetchItemData) error {
	switch item := item.(type) {
	case FetchItemDataSection:
		b, err := io.ReadAll(item.Literal)
		if err != nil {
			return err
		}
		if buf.Section == nil {
			buf.Section = make(map[FetchItem][]byte)
		}
		buf.Section[item.FetchItem()] = b
	case FetchItemDataFlags:
		buf.Flags = item.Flags
	case FetchItemDataEnvelope:
		buf.Envelope = item.Envelope
	case FetchItemDataInternalDate:
		buf.InternalDate = item.Time
	case FetchItemDataRFC822Size:
		buf.RFC822Size = item.Size
	case FetchItemDataUID:
		buf.UID = item.UID
	case FetchItemDataBodyStructure:
		buf.BodyStructure = item.BodyStructure
	default:
		panic(fmt.Errorf("unsupported fetch item data %T", item))
	}
	return nil
}

type startTLSConn struct {
	net.Conn
	r io.Reader
}

func (conn startTLSConn) Read(b []byte) (int, error) {
	return conn.r.Read(b)
}

// IdleCommand is an IDLE command.
//
// Initially, the IDLE command is running. The server may send unilateral
// data. The client cannot send any command while IDLE is running.
//
// Close must be called to stop the IDLE command.
type IdleCommand struct {
	cmd
	enc *commandEncoder
}

// Close stops the IDLE command.
//
// This method blocks until the command to stop IDLE is written, but doesn't
// wait for the server to respond. Callers can use Wait for this purpose.
func (cmd *IdleCommand) Close() error {
	if cmd.err != nil {
		return cmd.err
	}
	if cmd.enc == nil {
		return fmt.Errorf("imapclient: IDLE command closed twice")
	}
	_, err := cmd.enc.client.bw.WriteString("DONE\r\n")
	if err == nil {
		err = cmd.enc.client.bw.Flush()
	}
	cmd.enc.end()
	cmd.enc = nil
	return err
}

// Wait blocks until the IDLE command has completed.
//
// Wait can only be called after Close.
func (cmd *IdleCommand) Wait() error {
	if cmd.enc != nil {
		return fmt.Errorf("imapclient: IdleCommand.Close must be called before Wait")
	}
	return cmd.cmd.Wait()
}