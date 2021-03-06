package cbauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/couchbase/cbauth/cbauthimpl"
	"github.com/couchbase/cbauth/revrpc"
)

func newAuth(initPeriod time.Duration) *authImpl {
	return &authImpl{cbauthimpl.NewSVC(initPeriod, &DBStaleError{})}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func hashPassword(password string, salt []byte) []byte {
	h := hmac.New(sha1.New, salt)
	h.Write([]byte(password))
	return h.Sum(nil)
}

func mkUser(user, uType, password, salt string) (u cbauthimpl.User) {
	u.User = user
	u.Type = uType
	u.Salt = []byte(salt)
	u.Mac = hashPassword(password, u.Salt)
	return
}

func newAuthForTest(body func(freshChan chan struct{}, timeoutBody func())) *authImpl {
	testDur := 555 * time.Hour

	wf := func(period time.Duration, ch chan struct{}, timeoutBody func()) {
		if period != testDur {
			panic(period)
		}
		body(ch, timeoutBody)
	}

	return &authImpl{cbauthimpl.NewSVCForTest(testDur, &DBStaleError{}, wf)}
}

func acc(ok bool, err error) bool {
	must(err)
	return ok
}

func assertAdmins(t *testing.T, c Creds, needAdmin, needROAdmin bool) {
	if acc(c.IsAllowed("cluster.admin.settings!write")) != needAdmin {
		t.Fatalf("admin access must be: %v", needAdmin)
	}
	roadmin := !acc(c.IsAllowed("cluster.admin.settings!write")) &&
		acc(c.IsAllowed("cluster.admin.security!read"))
	if roadmin != needROAdmin {
		t.Fatalf("ro-admin access must be: %v", needROAdmin)
	}
}

func applyRT(rt *testingRoundTripper) func() {
	var old *http.Client
	old, http.DefaultClient = http.DefaultClient, &http.Client{Transport: rt}
	return func() {
		http.DefaultClient = old
	}
}

func newCache(a *authImpl) *cbauthimpl.Cache {
	url := "http://127.0.0.1:9000"
	return &cbauthimpl.Cache{
		AuthCheckURL:       url + "/_auth",
		PermissionCheckURL: url + "/_permissions",
	}
}

type testingRoundTripper struct {
	t       *testing.T
	baseURL string
	user    string
	source  string
	token   string
	tripped bool
}

func newTestingRT(t *testing.T) *testingRoundTripper {
	return &testingRoundTripper{
		t:       t,
		baseURL: "http://127.0.0.1:9000",
	}
}

func (rt *testingRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	path := strings.TrimPrefix(req.URL.String(), rt.baseURL)

	if req.URL.String() == path {
		log.Fatalf("Bad url: %v", req.URL)
	}

	switch {
	case req.Method == "POST" && path == "/_auth":
		return rt.authRoundTrip(req)
	case req.Method == "GET" && strings.HasPrefix(path, "/_permissions"):
		return rt.permissionsRoundTrip(req)
	}

	log.Fatalf("Unrecognized call, method: %s, path: %s", req.Method, path)
	return
}

func respond(req *http.Request, statusCode int, response string) *http.Response {
	respBody := ioutil.NopCloser(strings.NewReader(response))

	status := "None"
	switch statusCode {
	case 401:
		status = "401 Unauthorized"
	case 200:
		status = "200 OK"
	}

	return &http.Response{
		Status:        status,
		StatusCode:    statusCode,
		Proto:         "HTTP/1.0",
		ProtoMajor:    1,
		ProtoMinor:    0,
		Header:        http.Header{},
		Body:          respBody,
		ContentLength: -1,
		Trailer:       http.Header{},
		Request:       req,
	}
}

func (rt *testingRoundTripper) permissionsRoundTrip(req *http.Request) (res *http.Response, err error) {
	params := req.URL.Query()
	permission := params["permission"]
	user := params["user"]
	src := params["src"]

	if permission == nil || user == nil || src == nil {
		log.Fatalf("Missing parameters in request: %s", req.URL.String())
	}

	statusCode := 401

	switch src[0] {
	case "admin":
		statusCode = 200
	case "bucket":
		if permission[0] == "cluster.bucket["+user[0]+"].data!write" {
			statusCode = 200
		}
	case "anonymous":
		if permission[0] == "cluster.bucket[default].data!write" {
			statusCode = 200
		}
	}
	return respond(req, statusCode, ""), nil
}

func (rt *testingRoundTripper) authRoundTrip(req *http.Request) (res *http.Response, err error) {
	if rt.tripped {
		log.Fatalf("Already tripped")
	}

	rt.tripped = true

	statusCode := 200

	if req.Header.Get("ns-server-ui") == "yes" {
		token, err := req.Cookie("ui-auth-q")
		if err != nil || rt.token != token.Value {
			statusCode = 401
		}
	} else {
		log.Fatal("Expect to be called only with ns-server-ui=yes")
	}

	response := ""
	if statusCode == 200 {
		response = fmt.Sprintf(`{"user": "%s", "source": "%s"}`, rt.user, rt.source)
	}

	return respond(req, statusCode, response), nil
}

func (rt *testingRoundTripper) assertTripped(t *testing.T, expected bool) {
	if rt.tripped != expected {
		t.Fatalf("Tripped is not expected. Have: %v, need: %v", rt.tripped, expected)
	}
}

func (rt *testingRoundTripper) setTokenAuth(user, source, token string) {
	rt.token = token
	rt.source = source
	rt.user = user
}

func TestStaleBasic(t *testing.T) {
	for _, period := range []time.Duration{1, 0} {
		a := newAuth(period)
		_, err := a.Auth("asd", "bsd")
		if _, ok := err.(*DBStaleError); !ok {
			t.Fatalf("For period: %v expect ErrStale in stale state. Got %v", period, err)
		}
	}
}

func TestStale(t *testing.T) {
	sync1 := make(chan bool)
	sync2 := make(chan bool)

	go func() {
		sync1 <- true
		sync2 <- true
		close(sync2)
	}()

	a := newAuthForTest(func(ch chan struct{}, timeoutBody func()) {
		<-sync1
		go func() {
			<-sync2
			timeoutBody()
		}()
	})

	_, err := a.Auth("a", "b")
	if _, ok := err.(*DBStaleError); !ok {
		t.Fatalf("Expect ErrStale in stale state. Got %v", err)
	}

	if _, ok := <-sync2; ok {
		t.Fatal("Some bad sync")
	}

}

func doTestStaleThenAdmin(t *testing.T, updateBeforeTimer bool) {
	defer applyRT(newTestingRT(t))()

	timerchan := make(chan bool)
	var freshChan chan struct{}
	a := newAuthForTest(func(ch chan struct{}, timeoutBody func()) {
		freshChan = ch
		go func() {
			<-timerchan
			timeoutBody()
		}()
	})

	updatechan := make(chan bool)
	go func() {
		c := newCache(a)
		var users []cbauthimpl.User
		users = append(users, mkUser("admin", "admin", "asdasd", "nacl"))
		c.Users = users
		<-updatechan
		must(a.svc.UpdateDB(c, nil))
		<-updatechan
	}()

	go func() {
		freshChan <- struct{}{}
		if !updateBeforeTimer {
			close(timerchan)
			return
		}

		updatechan <- true
		updatechan <- true
		close(timerchan)
	}()

	cred, err := a.Auth("admin", "asdasd")
	if updateBeforeTimer {
		must(err)
		if ok, _ := cred.IsAllowed("cluster.admin.settings!write"); !ok {
			t.Fatal("user admin must be recognised as admin")
		}
	} else {
		if _, ok := err.(*DBStaleError); !ok {
			t.Fatal("db must be stale")
		}
		updatechan <- true
		updatechan <- true
	}

	if _, ok := <-timerchan; ok {
		t.Fatal("timerchan must be closed")
	}

	cred, err = a.Auth("admin", "badpass")
	if err != ErrNoAuth {
		t.Fatalf("badpass must not work. Instead got: %v and %v", cred, err)
	}

	cred, err = a.Auth("admin", "asdasd")
	must(err)
	if ok, _ := cred.IsAllowed("cluster.admin.settings!write"); !ok {
		t.Fatal("user admin must be recognised as admin")
	}
}

func TestStaleThenAdminTimerCase(t *testing.T) {
	doTestStaleThenAdmin(t, false)
}

func TestStaleThenAdminUpdateCase(t *testing.T) {
	doTestStaleThenAdmin(t, true)
}

func mkBucket(name, pwd string) (rv cbauthimpl.Bucket) {
	rv.Name = name
	rv.Password = pwd
	return
}

func canAccessBucket(c Creds, bucket string) bool {
	return acc(c.IsAllowed("cluster.bucket[" + bucket + "].data!write"))
}

func TestBucketsAuth(t *testing.T) {
	defer applyRT(newTestingRT(t))()

	a := newAuth(0)
	cache := newCache(a)
	cache.Buckets = append(cbauthimpl.Cache{}.Buckets, mkBucket("default", ""), mkBucket("foo", "bar"))
	must(a.svc.UpdateDB(cache, nil))

	c, err := a.Auth("foo", "bar")
	must(err)
	if !canAccessBucket(c, "foo") {
		t.Fatal("Expect foo access with right pw to work")
	}
	if canAccessBucket(c, "default") {
		t.Fatal("Expect default access to not work when authed towards foo")
	}
	if canAccessBucket(c, "unknown") {
		t.Fatal("Expect unknown bucket access to not work")
	}
	assertAdmins(t, c, false, false)

	c, err = a.Auth("foo", "notbar")
	if err != ErrNoAuth {
		t.Fatalf("Expect wrong password auth to fail. Got: %v and %v", c, err)
	}

	c, err = a.Auth("", "")
	must(err)
	assertAdmins(t, c, false, false)
	if canAccessBucket(c, "foo") {
		t.Fatal("Expect foo access to not work under anon auth")
	}
	if !canAccessBucket(c, "default") {
		t.Fatal("Expect default access to work under anon auth")
	}

	// now somebody deletes no-password default bucket
	must(a.svc.UpdateDB(&cbauthimpl.Cache{
		Buckets: append(cbauthimpl.Cache{}.Buckets,
			mkBucket("foo", "bar"))}, nil))
	c, err = a.Auth("foo", "bar")
	must(err)
	assertAdmins(t, c, false, false)
	if !canAccessBucket(c, "foo") {
		t.Fatal("Expect foo access to work under right pw")
	}
	// and no password access should not work
	c, err = a.Auth("", "")
	if err != ErrNoAuth {
		t.Fatalf("Expect no password access to fail after deletion of default bucket. Got: %v and %v", c, err)
	}
}

func mkNode(host, user, pwd string, ports []int, local bool) (rv cbauthimpl.Node) {
	rv.Host = host
	rv.User = user
	rv.Password = pwd
	rv.Ports = ports
	rv.Local = local
	return
}

func TestServicePwd(t *testing.T) {
	a := newAuth(0)
	c := cbauthimpl.Cache{
		Nodes: append(cbauthimpl.Cache{}.Nodes,
			mkNode("beta.local", "_admin", "foobar", []int{9000, 12000}, false),
			mkNode("chi.local", "_admin", "barfoo", []int{9001, 12001}, false)),
		SpecialUser: "@component",
	}

	must(a.svc.UpdateDB(&c, nil))
	u, p, err := a.GetMemcachedServiceAuth("unknown:9000")
	if _, ok := err.(UnknownHostPortError); u != "" || p != "" || !ok {
		t.Fatal("Expect error trying to get auth for unknown service")
	}
	u, p, _ = a.GetMemcachedServiceAuth("beta.local:9000")
	if u != "_admin" || p != "foobar" {
		t.Fatalf("Expect valid creds for beta.local:9000. Got: %s:%s", u, p)
	}
	u, p, _ = a.GetMemcachedServiceAuth("chi.local:12001")
	if u != "_admin" || p != "barfoo" {
		t.Fatalf("Expect valid creds for chi.local:12001. Got: %s:%s", u, p)
	}

	u, p, _ = a.GetHTTPServiceAuth("chi.local:9001")
	if u != "@component" || p != "barfoo" {
		t.Fatalf("Expect valid http creds for chi.local:9001. Got: %s:%s", u, p)
	}
}

func TestTokenAdmin(t *testing.T) {
	rt := newTestingRT(t)
	rt.setTokenAuth("Administrator", "admin", "1234567890")
	defer applyRT(rt)()

	a := newAuth(0)
	must(a.svc.UpdateDB(newCache(a), nil))

	req, err := http.NewRequest("GET", "http://q:11234/_queryStatsmaybe", nil)
	must(err)
	req.Header.Set("Cookie", "ui-auth-q=1234567890")
	req.Header.Set("ns-server-ui", "yes")

	c, err := a.AuthWebCreds(req)
	must(err)
	rt.assertTripped(t, true)

	assertAdmins(t, c, true, false)

	if c.Name() != "Administrator" {
		t.Errorf("Expect name to be Administrator")
	}

	if c.Source() != "ns_server" {
		t.Errorf("Expect source to be ns_server. Got %s", c.Source())
	}

	if !canAccessBucket(c, "asdasdasdasd") {
		t.Errorf("Expected to be able to access all buckets. Failed at asdasdasdasd")
	}
	if !canAccessBucket(c, "ffee") {
		t.Errorf("Expected to be able to access all buckets. Failed at ffee")
	}
}

func TestUnknownHostPortErrorFormatting(t *testing.T) {
	t.Log("Error: ", UnknownHostPortError("asdsd").Error())
}

func TestStaleErrorFormatting(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer s.Close()

	rpcsvc := revrpc.MustService(s.URL + "/test")
	a := newAuth(10 * time.Second)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		oldDef := revrpc.DefaultBabysitErrorPolicy.(revrpc.DefaultErrorPolicy)
		defer func() {
			revrpc.DefaultBabysitErrorPolicy = oldDef
		}()
		tmpDef := oldDef
		tmpDef.RestartsToExit = 1
		revrpc.DefaultBabysitErrorPolicy = tmpDef
		runRPCForSvc(rpcsvc, a.svc)
		wg.Done()
	}()

	_, err := a.Auth("", "")
	se, ok := err.(*DBStaleError)
	if !ok {
		t.Fatalf("Expected stale error. Got: %s", err)
	}
	errString := se.Error()
	t.Log("error string: ", errString)
	expectedString := "CBAuth database is stale: last reason: Need 200 status!. Got "
	if errString[:len(expectedString)] != expectedString {
		t.Fatalf("Expecting specific prefix of stale error. Got %s", errString)
	}
	wg.Wait()
}
