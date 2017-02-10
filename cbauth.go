// @author Couchbase <info@couchbase.com>
// @copyright 2014 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cbauth provides auth{N,Z} for couchbase server services.
package cbauth

import (
	"fmt"
	"net/http"

	"github.com/couchbase/cbauth/cbauthimpl"
)

// TODO: consider API that would allow us to do digest auth behind the
// scene

// TODO: for GetHTTPServiceAuth consider something more generic such
// as GetHTTPAuthHeader. Or even maybe RoundTrip. So that we can
// handle digest auth

// Authenticator is main cbauth interface. It supports both incoming
// and outgoing auth.
type Authenticator interface {
	// AuthWebCreds method extracts credentials from given http request.
	AuthWebCreds(req *http.Request) (creds Creds, err error)
	// Auth method constructs credentials from given user and password pair.
	Auth(user, pwd string) (creds Creds, err error)
	// GetHTTPServiceAuth returns user/password creds giving
	// "admin" access to given http service inside couchbase cluster.
	GetHTTPServiceAuth(hostport string) (user, pwd string, err error)
	// GetMemcachedServiceAuth returns user/password creds given
	// "admin" access to given memcached service.
	GetMemcachedServiceAuth(hostport string) (user, pwd string, err error)
}

// Creds type represents credentials and answers queries on this creds
// authorized actions. Note: it'll become (possibly much) wider API in
// future, but it's main purpose right now is to get us started.
type Creds interface {
	// Name method returns user name (e.g. for auditing)
	Name() string
	// Source method returns user source (for auditing)
	Source() string
	// IsAllowed method returns true if the permission is granted
	// for these credentials
	IsAllowed(permission string) (bool, error)
}

var _ Creds = (*cbauthimpl.CredsImpl)(nil)

type authImpl struct {
	svc *cbauthimpl.Svc
}

// DBStaleError is kind of error that signals that cbauth internal
// state is not synchronized with ns_server yet or anymore.
type DBStaleError struct {
	Err error
}

func (e *DBStaleError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("CBAuth database is stale: last reason: %s", e.Err)
	}
	return "CBAuth database is stale. Was never updated yet."
}

// ErrNoAuth is an error that is returned when the user credentials
// are not recognized
var ErrNoAuth = cbauthimpl.ErrNoAuth

// UnknownHostPortError is returned from GetMemcachedServiceAuth and
// GetHTTPServiceAuth calls for unknown host:port arguments.
type UnknownHostPortError string

func (s UnknownHostPortError) Error() string {
	return fmt.Sprintf("Unable to find given hostport in cbauth database: `%s'", string(s))
}

func (a *authImpl) AuthWebCreds(req *http.Request) (creds Creds, err error) {
	if cbauthimpl.IsAuthTokenPresent(req) {
		return cbauthimpl.VerifyOnServer(a.svc, req.Header)
	}
	user, pwd, err := ExtractCreds(req)
	if err != nil {
		return nil, err
	}
	return cbauthimpl.VerifyPassword(a.svc, user, pwd)
}

func (a *authImpl) Auth(user, pwd string) (creds Creds, err error) {
	return cbauthimpl.VerifyPassword(a.svc, user, pwd)
}

func (a *authImpl) GetMemcachedServiceAuth(hostport string) (user, pwd string, err error) {
	host, port, err := SplitHostPort(hostport)
	if err != nil {
		return "", "", err
	}
	user, _, pwd, err = cbauthimpl.GetCreds(a.svc, host, port)
	if err == nil && user == "" && pwd == "" {
		return "", "", UnknownHostPortError(hostport)
	}
	return
}

func (a *authImpl) GetHTTPServiceAuth(hostport string) (user, pwd string, err error) {
	host, port, err := SplitHostPort(hostport)
	if err != nil {
		return "", "", err
	}
	_, user, pwd, err = cbauthimpl.GetCreds(a.svc, host, port)
	if err == nil && user == "" && pwd == "" {
		return "", "", UnknownHostPortError(hostport)
	}
	return
}

var _ Authenticator = (*authImpl)(nil)
