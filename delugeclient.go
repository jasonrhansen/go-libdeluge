/*
 * go-libdeluge v0.1.0 - a native deluge RPC client library
 * Copyright (C) 2015~2016 gdm85 - https://github.com/gdm85/go-libdeluge/
This program is free software; you can redistribute it and/or
modify it under the terms of the GNU General Public License
as published by the Free Software Foundation; either version 2
of the License, or (at your option) any later version.
This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.
You should have received a copy of the GNU General Public License
along with this program; if not, write to the Free Software
Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301, USA.
*/
package delugeclient

import (
	"bytes"
	"compress/zlib"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/gdm85/go-rencode"
)

const (
	DefaultReadWriteTimeout = time.Second * 30
)

var (
	// ErrAlreadyClosed is returned when connection is already closed.
	ErrAlreadyClosed = errors.New("connection is already closed")
)

// Settings defines all settings for a Deluge client connection.
type Settings struct {
	Hostname         string
	Port             uint
	Login            string
	Password         string
	Logger           *log.Logger
	ReadWriteTimeout time.Duration // Timeout for read/write operations on the TCP stream.
}

// Client is a Deluge RPC client.
type Client struct {
	settings Settings
	conn     *tls.Conn
	serial   int64
	classID  int64
}

// RPCError is an error returned by RPC calls.
type RPCError struct {
	remoteMessage string
}

func (e RPCError) Error() string {
	return e.remoteMessage
}

type rpcResponseTypeID int

const (
	rpcResponse rpcResponseTypeID = 1
	rpcError    rpcResponseTypeID = 2
	rpcEvent    rpcResponseTypeID = 3
)

// DelugeResponse is a response returned from a completed RPC call.
type DelugeResponse struct {
	messageType rpcResponseTypeID
	requestID   int64
	// only for rpcResponse
	returnValue rencode.List
	// only in rpcError
	exceptionType    string
	exceptionMessage string
	traceBack        string
	// only in rpcEvent
	eventName string
	data      rencode.List
}

// IsError returns true when the response is an error.
func (dr *DelugeResponse) IsError() bool {
	return dr.messageType == rpcError
}

func (dr *DelugeResponse) String() string {
	switch dr.messageType {
	case rpcError:
		return fmt.Sprintf("RPC error %s('%s')\n%s", dr.exceptionType, dr.exceptionMessage, dr.traceBack)
	case rpcResponse:
		typeStr := ""
		for _, v := range dr.returnValue.Values() {
			typeStr += fmt.Sprintf("%T, ", v)
		}
		return fmt.Sprintf("%d return values [%s]", dr.returnValue.Length(), typeStr)
	}
	return fmt.Sprintf("invalid message type: %d", dr.messageType)
}

func (c *Client) resetTimeout() error {
	// set timeout
	return c.conn.SetDeadline(time.Now().Add(c.settings.ReadWriteTimeout))
}

func (c *Client) Rpc(methodName string, args rencode.List, kwargs rencode.Dictionary) (*DelugeResponse, error) {
	// generate serial
	c.serial++
	if c.serial == math.MaxInt64 {
		c.serial = 1
	}

	// rencode -> zlib -> openssl -> TCP
	b := bytes.Buffer{}
	z := zlib.NewWriter(&b)
	e := rencode.NewEncoder(z)

	// payload is wrapped twice in a list because there is support for multiple RPC calls
	// although not used currently
	payload := rencode.NewList(rencode.NewList(c.serial, methodName, args, kwargs))

	err := e.Encode(payload)
	if err != nil {
		return nil, err
	}

	// flush zlib-compressed buffer
	err = z.Close()
	if err != nil {
		return nil, err
	}
	if c.settings.Logger != nil {
		c.settings.Logger.Println("flushed zlib buffer")
	}

	// write to connection without closing it
	var n int
	n, err = c.conn.Write(b.Bytes())
	if err != nil {
		return nil, err
	}
	if c.settings.Logger != nil {
		//		c.settings.Logger.Println(hex.Dump(b.Bytes()))
		c.settings.Logger.Printf("written %d bytes to RPC connection", n)
	}

	err = c.resetTimeout()
	if err != nil {
		return nil, err
	}

	// setup a reader: TCP -> openssl -> zlib -> rencode -> {objects}
	zr, err := zlib.NewReader(c.conn)
	if err != nil {
		return nil, err
	}
	d := rencode.NewDecoder(zr)

	var respList rencode.List
	err = d.Scan(&respList)
	if err != nil {
		return nil, err
	}

	resp := DelugeResponse{}
	var mt int64
	err = respList.Scan(&mt, &resp.requestID)
	if err != nil {
		return nil, err
	}
	if resp.requestID != c.serial {
		return nil, errors.New("request/response serial id mismatch")
	}
	resp.messageType = rpcResponseTypeID(mt)

	// shift first two elements
	respList = rencode.NewList(respList.Values()[2:]...)

	switch resp.messageType {
	case rpcResponse:
		resp.returnValue = respList
	case rpcError:
		var errList rencode.List
		err = respList.Scan(&errList)
		if err != nil {
			return nil, err
		}
		err = errList.Scan(&resp.exceptionType, &resp.exceptionMessage, &resp.traceBack)
		if err != nil {
			return nil, err
		}
	case rpcEvent:
		return nil, errors.New("event support not available")
	default:
		return nil, errors.New("unknown message type")
	}

	if c.settings.Logger != nil {
		c.settings.Logger.Printf("RPC(%s) = %s\n", methodName, resp.String())
	}

	return &resp, nil
}

// New returns a Deluge client.
func New(s Settings) *Client {
	if s.ReadWriteTimeout == time.Duration(0) {
		s.ReadWriteTimeout = DefaultReadWriteTimeout
	}
	return &Client{
		settings: s,
	}
}

// Close closes the connection of a Deluge client.
func (c *Client) Close() error {
	if c.conn == nil {
		return ErrAlreadyClosed
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Connect performs connection to a Deluge daemon second previously specified settings.
func (c *Client) Connect() error {
	var err error
	c.conn, err = tls.Dial("tcp", fmt.Sprintf("%s:%d", c.settings.Hostname, c.settings.Port),
		&tls.Config{
			InsecureSkipVerify: true, // x509: cannot verify signature: algorithm unimplemented
		})
	if err != nil {
		return err
	}

	if c.settings.Logger != nil {
		c.settings.Logger.Printf("connected to %s:%d\n", c.settings.Hostname, c.settings.Port)
	}

	// perform login
	resp, err := c.Rpc("daemon.login", rencode.NewList(c.settings.Login, c.settings.Password), rencode.Dictionary{})
	if err != nil {
		return err
	}
	if resp.IsError() {
		return RPCError{resp.String()}
	}

	// get class of logged-in user
	err = resp.returnValue.Scan(&c.classID)
	if err != nil {
		return err
	}

	if c.settings.Logger != nil {
		c.settings.Logger.Println("login successful as user", c.settings.Login)
	}

	return nil
}

// MethodsList returns a list of available methods on server.
func (c *Client) MethodsList() ([]string, error) {
	resp, err := c.Rpc("daemon.get_method_list", rencode.List{}, rencode.Dictionary{})
	if err != nil {
		return []string{}, err
	}
	if resp.IsError() {
		return []string{}, RPCError{resp.String()}
	}

	var methodsList rencode.List
	err = resp.returnValue.Scan(&methodsList)
	if err != nil {
		return []string{}, err
	}
	result := make([]string, methodsList.Length())
	for i, m := range methodsList.Values() {
		result[i] = string(m.([]byte))
	}

	return result, nil
}

// DaemonVersion returns the running daemon version.
func (c *Client) DaemonVersion() (string, error) {
	resp, err := c.Rpc("daemon.info", rencode.List{}, rencode.Dictionary{})
	if err != nil {
		return "", err
	}
	if resp.IsError() {
		return "", RPCError{resp.String()}
	}

	var info string
	err = resp.returnValue.Scan(&info)
	if err != nil {
		return "", err
	}

	return info, nil
}

type Options map[string]interface{}

func mapToRencodeDictionary(m map[string]interface{}) rencode.Dictionary {
	var dict rencode.Dictionary
	for k, v := range m {
		dict.Add(k, v)
	}

	return dict
}

func sliceToRencodeList(s []string) rencode.List {
	var list rencode.List
	for _, v := range s {
		list.Add(v)
	}

	return list
}

// AddTorrentMagnet adds a torrent via magnet URI and returns the torrent hash.
func (c *Client) AddTorrentMagnet(magnetURI string, options Options) (string, error) {
	var args rencode.List
	args.Add(magnetURI, mapToRencodeDictionary(options))

	resp, err := c.Rpc("core.add_torrent_magnet", args, rencode.Dictionary{})
	if err != nil {
		return "", err
	}
	if resp.IsError() {
		return "", RPCError{resp.String()}
	}

	// returned hash may be nil if torrent was already added
	torrentHash, err := resp.returnValue.Get(0)
	if err != nil {
		return "", err
	}
	if torrentHash == nil {
		return "", nil
	}
	return string(torrentHash.([]uint8)), nil
}

// AddTorrentURL adds a torrent via a URL and returns the torrent hash.
func (c *Client) AddTorrentURL(url string, options Options) (string, error) {
	var args rencode.List
	args.Add(url, mapToRencodeDictionary(options))

	resp, err := c.Rpc("core.add_torrent_url", args, rencode.Dictionary{})
	if err != nil {
		return "", err
	}
	if resp.IsError() {
		return "", RPCError{resp.String()}
	}

	// returned hash may be nil if torrent was already added
	torrentHash, err := resp.returnValue.Get(0)
	if err != nil {
		return "", err
	}
	if torrentHash == nil {
		return "", nil
	}
	return string(torrentHash.([]uint8)), nil
}

func (c *Client) MoveStorage(torrentIDs []string, dest string) error {
	var args rencode.List
	args.Add(sliceToRencodeList(torrentIDs), dest)

	resp, err := c.Rpc("core.move_storage", args, rencode.Dictionary{})
	if err != nil {
		return err
	}
	if resp.IsError() {
		return RPCError{resp.String()}
	}

	return err
}

func (c *Client) RemoveTorrent(torrentID string, removeData bool) (bool, error) {
	var args rencode.List
	args.Add(torrentID, removeData)

	resp, err := c.Rpc("core.remove_torrent", args, rencode.Dictionary{})
	if err != nil {
		return false, err
	}
	if resp.IsError() {
		return false, RPCError{resp.String()}
	}

	return true, nil
}

func (c *Client) GetTorrentStatus(torrentID string, keys []string, diff bool) (map[string]interface{}, error) {
	var args rencode.List
	args.Add(torrentID, sliceToRencodeList(keys), diff)
	resp, err := c.Rpc("core.get_torrent_status", args, rencode.Dictionary{})
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, RPCError{resp.String()}
	}

	var statusDict rencode.Dictionary
	err = resp.returnValue.Scan(&statusDict)
	if err != nil {
		return nil, err
	}

	result := make(map[string]interface{}, statusDict.Length())
	for i := 0; i < statusDict.Length(); i++ {
		key := string(statusDict.Keys()[i].([]byte))
		var value interface{}
		value = statusDict.Values()[i]
		if v, ok := value.([]byte); ok {
			value = string(v)
		}
		result[key] = value
	}

	return result, nil
}

func (c *Client) GetTorrentsStatus(filter map[string][]string, keys []string, diff bool) (map[string]interface{}, error) {
	log.Printf("GetTorrentsStatus filter: %#v \n", filter)
	var filterDict rencode.Dictionary
	for k, v := range filter {
		filterDict.Add(k, sliceToRencodeList(v))
	}

	var args rencode.List
	args.Add(filterDict, sliceToRencodeList(keys), diff)
	log.Printf("GetTorrentsStatus args: %#v \n", args)
	resp, err := c.Rpc("core.get_torrents_status", args, rencode.Dictionary{})
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, RPCError{resp.String()}
	}

	var resultDict rencode.Dictionary
	err = resp.returnValue.Scan(&resultDict)
	if err != nil {
		return nil, err
	}
	log.Printf("GetTorrentsStatus resultDict: %#v \n", resultDict)
	result := make(map[string]interface{}, resultDict.Length())
	for i := 0; i < resultDict.Length(); i++ {
		tid := string(resultDict.Keys()[i].([]byte))

		statusDict := resultDict.Values()[i].(rencode.Dictionary)
		statusMap := make(map[string]interface{}, statusDict.Length())
		for j := 0; j < statusDict.Length(); j++ {
			key := string(statusDict.Keys()[j].([]byte))
			var value interface{}
			value = statusDict.Values()[j]
			if v, ok := value.([]byte); ok {
				value = string(v)
			}
			statusMap[key] = value
		}

		result[tid] = statusMap
	}

	log.Printf("GetTorrentsStatus result: %#v \n", result)

	return result, nil
}

func (c *Client) GetSessionState() ([]string, error) {
	resp, err := c.Rpc("core.get_session_state", rencode.List{}, rencode.Dictionary{})
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, RPCError{resp.String()}
	}

	var idList rencode.List
	err = resp.returnValue.Scan(&idList)
	if err != nil {
		return []string{}, err
	}
	result := make([]string, idList.Length())
	for i, m := range idList.Values() {
		result[i] = string(m.([]byte))
	}

	return result, nil
}
