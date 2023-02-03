// Copyright 2022-present The ZTDBP Authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//     http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"

	"mysqlproxy/packet"

	"github.com/siddontang/go/sync2"
)

/*
   Conn acts like a MySQL server connection, you can use MySQL client to communicate with it.
*/
type Conn struct {
	*packet.Conn

	serverConf     *Server
	capability     uint32
	charset        uint8
	authPluginName string
	connectionID   uint32
	status         uint16
	warnings       uint16
	salt           []byte // should be 8 + 12 for auth-plugin-data-part-1 and auth-plugin-data-part-2

	credentialProvider  CredentialProvider
	user                string
	password            string
	db                  string
	cachingSha2FullAuth bool

	h Handler

	stmts  map[uint32]*Stmt
	stmtID uint32

	closed sync2.AtomicBool
}

var baseConnID uint32 = 10000

// NewConn: create connection with default server settings
func NewConn(conn net.Conn, user string, password string, h Handler) (*Conn, error) {
	p := NewInMemoryProvider()
	p.AddUser(user, password)
	salt, _ := RandomBuf(20)
	var packetConn *packet.Conn
	if defaultServer.tlsConfig != nil {
		packetConn = packet.NewTLSConn(conn)
	} else {
		packetConn = packet.NewConn(conn)
	}

	c := &Conn{
		Conn:               packetConn,
		serverConf:         defaultServer,
		credentialProvider: p,
		h:                  h,
		connectionID:       atomic.AddUint32(&baseConnID, 1),
		stmts:              make(map[uint32]*Stmt),
		salt:               salt,
	}
	c.closed.Set(false)

	if err := c.handshake(); err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// NewCustomizedConn: create connection with customized server settings
func NewCustomizedConn(conn net.Conn, serverConf *Server, p CredentialProvider, beforeOK func(*Conn) error) (*Conn, error) {
	var packetConn *packet.Conn
	if serverConf.tlsConfig != nil {
		packetConn = packet.NewTLSConn(conn)
	} else {
		packetConn = packet.NewConn(conn)
	}

	salt, _ := RandomBuf(20)
	c := &Conn{
		Conn:               packetConn,
		serverConf:         serverConf,
		credentialProvider: p,
		connectionID:       atomic.AddUint32(&baseConnID, 1),
		stmts:              make(map[uint32]*Stmt),
		salt:               salt,
	}
	c.closed.Set(false)

	if err := c.HandshakeOK(beforeOK); err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// NewCustomizedConn: create connection with customized server settings
func NewClientConn(conn net.Conn, serverConf *Server, p CredentialProvider) *Conn {
	var packetConn *packet.Conn
	if serverConf.tlsConfig != nil {
		packetConn = packet.NewTLSConn(conn)
	} else {
		packetConn = packet.NewConn(conn)
	}

	salt, _ := RandomBuf(20)
	c := &Conn{
		Conn:               packetConn,
		serverConf:         serverConf,
		credentialProvider: p,
		connectionID:       atomic.AddUint32(&baseConnID, 1),
		stmts:              make(map[uint32]*Stmt),
		salt:               salt,
	}
	c.closed.Set(false)

	return c
}

func (c *Conn) HandshakeOK(fn func(c *Conn) error) error {
	if err := c.writeInitialHandshake(); err != nil {
		return err
	}
	if err := c.readHandshakeResponse(); err != nil {
		if errors.Is(err, ErrAccessDenied) {
			var usingPasswd uint16 = ER_YES
			if errors.Is(err, ErrAccessDeniedNoPassword) {
				usingPasswd = ER_NO
			}
			err = NewDefaultError(ER_ACCESS_DENIED_ERROR, fmt.Sprintf("%s:%s", c.user, c.password), c.RemoteAddr().String(), MySQLErrName[usingPasswd])
		}
		_ = c.writeError(err)
		return err
	}

	if err := fn(c); err != nil {
		nerr := NewError(ER_SIGNAL_EXCEPTION, fmt.Sprintf("Can't connect to remote MySQL server: %s@tcp(%s)/%s", c.user, c.Conn.Conn.LocalAddr(), c.db))
		_ = c.writeError(nerr)
		return err
	}

	if err := c.writeOK(nil); err != nil {
		return err
	}

	c.ResetSequence()

	return nil
}

func (c *Conn) Handshake(hp Handler) error {
	c.h = hp
	if err := c.handshake(); err != nil {
		c.Close()
		return err
	}
	return nil
}

func (c *Conn) handshake() error {
	if err := c.writeInitialHandshake(); err != nil {
		return err
	}

	if err := c.readHandshakeResponse(); err != nil {
		if errors.Is(err, ErrAccessDenied) {
			var usingPasswd uint16 = ER_YES
			if errors.Is(err, ErrAccessDeniedNoPassword) {
				usingPasswd = ER_NO
			}
			err = NewDefaultError(ER_ACCESS_DENIED_ERROR, c.user, c.RemoteAddr().String(), MySQLErrName[usingPasswd])
		}
		_ = c.writeError(err)
		return err
	}

	if err := c.writeOK(nil); err != nil {
		return err
	}

	c.ResetSequence()

	return nil
}

func (c *Conn) Close() {
	c.closed.Set(true)
	c.Conn.Close()
}

func (c *Conn) Closed() bool {
	return c.closed.Get()
}

func (c *Conn) GetUser() string {
	return c.user
}

func (c *Conn) Capability() uint32 {
	return c.capability
}

func (c *Conn) SetCapability(cap uint32) {
	c.capability |= cap
}

func (c *Conn) UnsetCapability(cap uint32) {
	c.capability &= ^cap
}

func (c *Conn) HasCapability(cap uint32) bool {
	return c.capability&cap > 0
}

func (c *Conn) Charset() uint8 {
	return c.charset
}

func (c *Conn) ConnectionID() uint32 {
	return c.connectionID
}

func (c *Conn) IsAutoCommit() bool {
	return c.HasStatus(SERVER_STATUS_AUTOCOMMIT)
}

func (c *Conn) IsInTransaction() bool {
	return c.HasStatus(SERVER_STATUS_IN_TRANS)
}

func (c *Conn) SetInTransaction() {
	c.SetStatus(SERVER_STATUS_IN_TRANS)
}

func (c *Conn) ClearInTransaction() {
	c.UnsetStatus(SERVER_STATUS_IN_TRANS)
}

func (c *Conn) SetStatus(status uint16) {
	c.status |= status
}

func (c *Conn) UnsetStatus(status uint16) {
	c.status &= ^status
}

func (c *Conn) HasStatus(status uint16) bool {
	return c.status&status > 0
}

func (c *Conn) SetWarnings(warnings uint16) {
	c.warnings = warnings
}
