package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func hash(b string) string { v := sha256.Sum256([]byte(b)); return hex.EncodeToString(v[:]) }
func TestMaskEveryBoundaryAndIndependentPipes(t *testing.T) {
	for split := 0; split <= 12; split++ {
		r := &Registry{}
		if e := r.Register("secret", "secretlong", "cret"); e != nil {
			t.Fatal(e)
		}
		a, b := r.Pipe(), r.Pipe()
		src := []byte("xxsecretlongyy")
		out := append(a.Feed(src[:split], false), a.Feed(src[split:], true)...)
		if string(out) != "xx**********yy" {
			t.Fatalf("split %d: %q", split, out)
		}
		if string(b.Feed([]byte("secret"), true)) != "******" {
			t.Fatal("stderr masking")
		}
	}
}
func TestRegistryCapacityAtomic(t *testing.T) {
	r := &Registry{}
	if r.Register(strings.Repeat("x", 262144), "overflow") == nil || r.total != 0 {
		t.Fatal("partial registration")
	}
	if r.Register("") == nil {
		t.Fatal("empty secret")
	}
}
func TestDrainOverflowAndCursor(t *testing.T) {
	r := &Registry{}
	r.Register("secret")
	q := &Queue{}
	data := strings.Repeat("x", WorkerBuffer+100)
	if e := Drain(strings.NewReader(data), r.Pipe(), q.Append); e != nil {
		t.Fatal(e)
	}
	s := q.Take()
	var end int64
	gaps := false
	for _, v := range s {
		if v.Start != end {
			t.Fatal("lost range")
		}
		end = v.End
		gaps = gaps || v.Gap != nil
	}
	if !gaps || end != int64(len(data)) {
		t.Fatal("overflow not explicit")
	}
	c := Cursor{Attempt: "a", Stream: "console", Epoch: "e", Next: 1}
	if _, e := DecodeCursor(c.Encode(), "b"); e == nil {
		t.Fatal("cross-attempt cursor")
	}
	if _, e := DecodeCursor(strings.Repeat("x", 1025), "a"); e == nil {
		t.Fatal("unbounded cursor")
	}
}
func TestPreviewReplayAndReset(t *testing.T) {
	i := 0
	p := NewPreviews(func() string { i++; return fmt.Sprint(i) })
	text := "hello"
	s := Segment{Sequence: 0, End: 5, Text: &text}
	epoch, e := p.Append("a", "worker", s)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Append("a", "worker", s); e != nil {
		t.Fatal(e)
	}
	changed := "other"
	s.Text = &changed
	if _, e = p.Append("a", "worker", s); e == nil {
		t.Fatal("mismatched replay")
	}
	for j := 1; j < 20; j++ {
		v := strings.Repeat("a", 65536)
		p.Append("a", "worker", Segment{Sequence: int64(j), Start: int64(j * 65536), End: int64((j + 1) * 65536), Text: &v})
	}
	next, _ := p.Snapshot("a")
	if next == epoch {
		t.Fatal("eviction kept epoch")
	}
}
func TestSpoolIntegrity(t *testing.T) {
	for _, v := range []string{"sho", "longer", "wrong"} {
		if f, e := spool(strings.NewReader(v), 4, hash("good")); e == nil {
			f.Close()
			os.Remove(f.Name())
			t.Fatal("bad length/hash accepted")
		}
	}
	f, e := spool(strings.NewReader("good"), 4, hash("good"))
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(f.Name())
	f.Close()
}

// Endpoints are injected into the SDK only inside tests; production has no
// endpoint override, fixture credential chain, or relaxed bucket validator.
func TestSyntheticS3ExactVersionAndConfiguration(t *testing.T) {
	version, encrypted, public := "retained", "AES256", false
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Expected-Bucket-Owner") != "123456789012" {
			t.Error("owner fence missing")
		}
		switch {
		case r.Method == "HEAD":
			w.Header().Set("X-Amz-Bucket-Region", "eu-west-1")
		case r.URL.Query().Has("versioning"):
			fmt.Fprint(w, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
		case r.URL.Query().Has("encryption"):
			fmt.Fprintf(w, `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>%s</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`, encrypted)
		case r.URL.Query().Has("publicAccessBlock"):
			fmt.Fprintf(w, `<PublicAccessBlockConfiguration><BlockPublicAcls>%t</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`, !public)
		case r.Method == "PUT":
			puts++
			io.Copy(io.Discard, r.Body)
			w.Header().Set("X-Amz-Version-Id", "retained")
		default:
			if r.URL.Query().Get("versionId") != "retained" {
				t.Error("latest read")
			}
			w.Header().Set("X-Amz-Version-Id", version)
			w.Header().Set("X-Amz-Server-Side-Encryption", "AES256")
			w.Header().Set("Content-Length", "4")
			fmt.Fprint(w, "good")
		}
	}))
	defer srv.Close()
	sdk := s3.NewFromConfig(aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	st := &S3{sdk, Settings{"fixture", "eu-west-1", "123456789012"}}
	if e := st.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	if v, e := st.Put(context.Background(), "key", bytes.NewReader([]byte("good")), 4); e != nil || v != "retained" || puts != 1 {
		t.Fatal(v, e)
	}
	c := &Custody{st, "fixture"}
	a := Artifact{Bucket: "fixture", Key: "key", Version: "retained", Hash: hash("good"), Size: 4, Kind: "manifest"}
	f, e := c.verified(context.Background(), a)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	os.Remove(f.Name())
	version = "wrong"
	if _, e = c.verified(context.Background(), a); e == nil {
		t.Fatal("wrong exact version")
	}
	version = "retained"
	encrypted = "aws:kms"
	if st.Check(context.Background()) == nil {
		t.Fatal("wrong encryption")
	}
	encrypted = "AES256"
	public = true
	if st.Check(context.Background()) == nil {
		t.Fatal("public bucket")
	}
}
func TestMultipartUsesBoundedParts(t *testing.T) {
	var sizes []int64
	aborted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Expected-Bucket-Owner") != "123456789012" {
			t.Error("missing owner")
		}
		switch {
		case r.Method == "POST" && r.URL.Query().Has("uploads"):
			fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT":
			n, _ := io.Copy(io.Discard, r.Body)
			sizes = append(sizes, n)
			w.Header().Set("ETag", "part")
		case r.Method == "DELETE":
			aborted = true
		default:
			w.Header().Set("X-Amz-Version-Id", "exact")
			fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>done</ETag></CompleteMultipartUploadResult>`)
		}
	}))
	defer srv.Close()
	sdk := s3.NewFromConfig(aws.Config{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	st := &S3{sdk, Settings{"fixture", "eu-west-1", "123456789012"}}
	v, e := st.Put(context.Background(), "key", strings.NewReader(strings.Repeat("x", PartSize+10)), PartSize+10)
	if e != nil || v != "exact" || len(sizes) != 2 || sizes[0] != PartSize || sizes[1] != 10 || aborted {
		t.Fatal(v, e, sizes, aborted)
	}
}
func TestProductionStorageRejectsConfigurationWithoutCredentialLookup(t *testing.T) {
	for _, s := range []Settings{{}, {"bucket", "eu-west-1", "wrong"}, {"bucket", "http://localhost:9000", "123456789012"}, {"127.0.0.1", "eu-west-1", "123456789012"}} {
		if _, e := NewS3(context.Background(), s); e == nil {
			t.Fatal("invalid settings")
		}
	}
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:9000")
	t.Setenv("AWS_ACCESS_KEY_ID", "never-read")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "never-read")
	s, e := NewS3(context.Background(), Settings{"fixture", "eu-west-1", "123456789012"})
	if e != nil || s.client.Options().BaseEndpoint != nil {
		t.Fatal("production endpoint override")
	}
}
func TestQueueAcknowledgesOnlyExactInFlightSegment(t *testing.T) {
	q := &Queue{}
	q.Append([]byte(strings.Repeat("a", WorkerBuffer+10)))
	for {
		s, ok := q.Peek()
		if !ok {
			t.Fatal("no gap")
		}
		if s.Gap != nil {
			q.Append([]byte("more-loss"))
			if e := q.Acknowledge(s.Sequence); e != nil {
				t.Fatal(e)
			}
			next, ok := q.Peek()
			if !ok || next.Start != s.End || next.End != s.End+9 {
				t.Fatal("in-flight gap was extended or lost", next)
			}
			break
		}
		if q.Acknowledge(s.Sequence+1) == nil {
			t.Fatal("wrong acknowledgment")
		}
		q.Acknowledge(s.Sequence)
	}
}
func TestUTF8Bounds(t *testing.T) {
	if _, e := TextPrefix("€", 1); e == nil {
		t.Fatal("split encoded character")
	}
	v, e := TextPrefix("a€z", 4)
	if e != nil || v != "a€" {
		t.Fatal(v, e)
	}
	q := &Queue{}
	q.Append(bytes.Repeat([]byte{255}, WorkerBuffer))
	if q.bytes > WorkerBuffer {
		t.Fatal("invalid UTF8 exceeded buffer cap")
	}
}
