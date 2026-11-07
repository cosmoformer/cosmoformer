//go:build integration

package acceptance

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Deliberately small wire fixtures, not an AWS emulator. Unknown requests fail
// the test instead of receiving a generic success response.
type nativeEvent struct {
	Service, Method, Key, Condition string
	Conflict                        bool
}
type nativeBackend struct {
	t                    *testing.T
	server               *httptest.Server
	mu                   sync.Mutex
	objects              map[string][]byte
	items                map[string]map[string]map[string]string
	events               []nativeEvent
	owner, expectedOwner string
	ownerDenials         int
}

const nativeBucket = "fictional-state-bucket"
const nativeTable = "fictional-locks"

func newNativeBackend(t *testing.T) *nativeBackend {
	b := &nativeBackend{t: t, objects: map[string][]byte{}, items: map[string]map[string]map[string]string{}, owner: "111122223333"}
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.server.Close)
	return b
}
func (b *nativeBackend) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture/") {
		b.t.Error("fixture received unexpected signing identity")
		http.Error(w, "invalid identity", 403)
		return
	}
	if r.Header.Get("X-Amz-Target") != "" {
		b.dynamo(w, r)
		return
	}
	if r.URL.Path != "/"+nativeBucket && !strings.HasPrefix(r.URL.Path, "/"+nativeBucket+"/") {
		b.t.Errorf("unexpected S3 path: %s", r.URL.Path)
		http.Error(w, "invalid bucket", 400)
		return
	}
	// Model the generated session policy's explicit resource-owner Deny. This
	// fixture is not an IAM evaluator or evidence about live AWS permissions.
	if b.expectedOwner != "" && b.owner != b.expectedOwner {
		b.ownerDenials++
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>fixture owner policy denied</Message></Error>")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+nativeBucket+"/")
	if r.URL.Query().Get("list-type") == "2" && r.Method == "GET" {
		b.events = append(b.events, nativeEvent{Service: "s3", Method: "LIST", Key: r.URL.Query().Get("prefix")})
		type object struct {
			Key string `xml:"Key"`
		}
		type listing struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Name        string
			Prefix      string
			KeyCount    int
			MaxKeys     int
			IsTruncated bool
			Contents    []object
		}
		out := listing{Name: nativeBucket, Prefix: r.URL.Query().Get("prefix"), MaxKeys: 1000}
		keys := []string{}
		for k := range b.objects {
			if strings.HasPrefix(k, out.Prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Contents = append(out.Contents, object{k})
		}
		out.KeyCount = len(keys)
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(out)
		return
	}
	ev := nativeEvent{Service: "s3", Method: r.Method, Key: key, Condition: r.Header.Get("If-None-Match")}
	data, exists := b.objects[key]
	switch r.Method {
	case "GET", "HEAD":
		b.events = append(b.events, ev)
		if !exists {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(404)
			if r.Method == "GET" {
				io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			}
			return
		}
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
		if r.Method == "GET" {
			w.Write(data)
		}
	case "PUT":
		if ev.Condition != "" && ev.Condition != "*" {
			b.t.Error("unknown S3 write precondition")
		}
		if ev.Condition == "*" && exists {
			ev.Conflict = true
			b.events = append(b.events, ev)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(412)
			io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			return
		}
		if r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
			b.t.Error("unencrypted state/lock write")
		}
		var reader io.Reader = r.Body
		if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
			reader = httputil.NewChunkedReader(r.Body)
		}
		payload, e := io.ReadAll(io.LimitReader(reader, 8<<20+1))
		if e != nil || len(payload) > 8<<20 {
			b.t.Error("invalid state/lock body", e)
			http.Error(w, "body", 400)
			return
		}
		b.objects[key] = payload
		b.events = append(b.events, ev)
		sum := md5.Sum(payload)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(200)
	case "DELETE":
		delete(b.objects, key)
		b.events = append(b.events, ev)
		w.WriteHeader(204)
	default:
		b.t.Errorf("unexpected S3 operation %s", r.Method)
		http.Error(w, "unsupported", 400)
	}
}

func (b *nativeBackend) ownerPolicy(backend source.Backend) {
	b.t.Helper()
	raw, e := backend.OwnerSessionPolicy()
	if e != nil {
		b.t.Fatal(e)
	}
	var deny struct {
		Effect    string
		Action    string
		Resource  []string
		Condition map[string]map[string]string
	}
	// The Allow statement has a scalar Resource; inspect only the Deny from the
	// complete JSON after the production unit tests verify its exact structure.
	var envelope struct {
		Statements []json.RawMessage `json:"Statement"`
	}
	if e = json.Unmarshal(raw, &envelope); e != nil || len(envelope.Statements) != 2 {
		b.t.Fatal("invalid owner policy", e)
	}
	if e = json.Unmarshal(envelope.Statements[1], &deny); e != nil {
		b.t.Fatal(e)
	}
	owner := deny.Condition["StringNotEquals"]["s3:ResourceAccount"]
	if deny.Effect != "Deny" || deny.Action != "s3:*" || owner == "" || len(deny.Resource) != 2 || deny.Resource[0] != "arn:"+backend.Target.Partition+":s3:::"+nativeBucket {
		b.t.Fatal("incorrect fixture owner policy")
	}
	b.expectedOwner = owner
}
func (b *nativeBackend) dynamo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Table      string `json:"TableName"`
		Item       map[string]map[string]string
		Key        map[string]map[string]string
		Condition  string `json:"ConditionExpression"`
		Projection string `json:"ProjectionExpression"`
		Consistent bool   `json:"ConsistentRead"`
	}
	if r.Method != "POST" || json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || req.Table != nativeTable {
		b.t.Error("invalid DynamoDB fixture request")
		http.Error(w, "invalid request", 400)
		return
	}
	method := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "DynamoDB_20120810.")
	key := req.Key["LockID"]["S"]
	if method == "PutItem" {
		key = req.Item["LockID"]["S"]
	}
	if !strings.HasPrefix(key, nativeBucket+"/") {
		b.t.Error("invalid native LockID", key)
	}
	ev := nativeEvent{Service: "ddb", Method: method, Key: key, Condition: req.Condition}
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	switch method {
	case "PutItem":
		if _, exists := b.items[key]; req.Condition != "" && exists {
			ev.Conflict = true
			b.events = append(b.events, ev)
			w.WriteHeader(400)
			io.WriteString(w, `{"__type":"com.amazonaws.dynamodb.v20120810#ConditionalCheckFailedException","message":"fixture contention"}`)
			return
		}
		if strings.HasSuffix(key, "-md5") {
			digest := req.Item["Digest"]["S"]
			raw, e := hex.DecodeString(digest)
			if e != nil || len(raw) != 16 || req.Condition != "" {
				b.t.Error("invalid native checksum item")
			}
		} else if req.Condition != "attribute_not_exists(LockID)" || !json.Valid([]byte(req.Item["Info"]["S"])) {
			b.t.Error("invalid conditional lock item")
		}
		b.items[key] = req.Item
		io.WriteString(w, "{}")
	case "GetItem":
		if !req.Consistent {
			b.t.Error("lock/checksum read not consistent")
		}
		if item, ok := b.items[key]; ok {
			json.NewEncoder(w).Encode(map[string]any{"Item": item})
		} else {
			io.WriteString(w, "{}")
		}
	case "DeleteItem":
		delete(b.items, key)
		io.WriteString(w, "{}")
	default:
		b.t.Errorf("unexpected DynamoDB operation %s", method)
		http.Error(w, "unsupported", 400)
	}
	b.events = append(b.events, ev)
}
func (b *nativeBackend) snapshot() (map[string][]byte, []nativeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := map[string][]byte{}
	for k, v := range b.objects {
		m[k] = bytes.Clone(v)
	}
	return m, append([]nativeEvent(nil), b.events...)
}
func (b *nativeBackend) replaceState(key string, change func(map[string]any)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var state map[string]any
	if e := json.Unmarshal(b.objects[key], &state); e != nil {
		b.t.Fatal(e)
	}
	change(state)
	raw, e := json.Marshal(state)
	if e != nil {
		b.t.Fatal(e)
	}
	b.objects[key] = raw
	if item := b.items[nativeBucket+"/"+key+"-md5"]; item != nil {
		sum := md5.Sum(raw)
		item["Digest"] = map[string]string{"S": hex.EncodeToString(sum[:])}
	}
}
func (b *nativeBackend) contend(key, service string) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	info := fmt.Sprintf(`{"ID":"11111111-2222-3333-4444-555555555555","Operation":"OperationTypePlan","Who":"fixture","Version":"fixture","Created":"2026-01-01T00:00:00Z","Path":%q}`, nativeBucket+"/"+key)
	if service == "s3" {
		b.objects[key+".tflock"] = []byte(info)
	} else {
		b.items[nativeBucket+"/"+key] = map[string]map[string]string{"LockID": {"S": nativeBucket + "/" + key}, "Info": {"S": info}}
	}
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.objects, key+".tflock")
		delete(b.items, nativeBucket+"/"+key)
	}
}
