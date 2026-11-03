// Package api serves the registry over HTTP as JSON.
//
// The API is read-mostly: the only write is registering or refreshing a
// contract, which does not change the contract itself, only what SoroVault
// knows about it. There is no authentication by default — deployment is
// expected to put the service behind whatever the operator already uses —
// and when API tokens are configured, write endpoints require a bearer
// credential while reads stay public.
package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/soroworks/sorovault/internal/attest"
	"github.com/soroworks/sorovault/internal/codegen"
	"github.com/soroworks/sorovault/internal/model"
	"github.com/soroworks/sorovault/internal/refresher"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
	"github.com/soroworks/sorovault/internal/wasm"
)

// defaultStreamHeartbeat is how often an idle SSE stream emits a comment to
// keep intermediate proxies from closing the connection.
const defaultStreamHeartbeat = 15 * time.Second

// Server holds the API's dependencies.
type Server struct {
	reg       *registry.Registry
	log       *slog.Logger
	cache     *interfaceCache
	rateLimit *clientLimiter

	broker          *refresher.Broker
	streamHeartbeat time.Duration

	// lastSweep, when set, reports the most recent automatic refresh
	// sweep. It is nil when refresh is disabled, and GET /api/stats then
	// omits the field.
	lastSweep func() (refresher.SweepRecord, bool)

	// signer, when set, signs interface responses. keys is every public key
	// published at GET /api/keys: the signer's, then any retired ones.
	signer *attest.Signer
	keys   []attest.Key

	// readOnly marks a mirror that refuses writes. It is read once, when
	// Routes mounts, so a server's mode is fixed at startup.
	readOnly bool

	// tokens are the bearer credentials writes must present. Empty keeps
	// every write public, as the API has always behaved; they are held
	// here and compared in constant time, and never logged or echoed.
	tokens []string
}

// NewServer builds an API server over a registry.
func NewServer(reg *registry.Registry, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		reg:             reg,
		log:             log,
		cache:           newInterfaceCache(interfaceCacheBytes),
		broker:          refresher.NewBroker(0, 0),
		streamHeartbeat: defaultStreamHeartbeat,
	}
}

// SetBroker attaches the upgrade fan-out broker that backs GET /api/upgrades/stream.
// A nil broker is ignored so the server always has one.
func (s *Server) SetBroker(b *refresher.Broker) {
	if b != nil {
		s.broker = b
	}
}

// Broker returns the server's upgrade fan-out broker.
func (s *Server) Broker() *refresher.Broker {
	return s.broker
}

// SetStreamMaxSubscribers configures the maximum number of concurrent SSE
// clients on GET /api/upgrades/stream.
func (s *Server) SetStreamMaxSubscribers(maxSubscribers int) {
	if s.broker != nil {
		s.broker.SetMaxSubscribers(maxSubscribers)
	}
}

// SetStreamHeartbeat configures how often an idle SSE connection emits a
// heartbeat comment. Non-positive values are ignored.
func (s *Server) SetStreamHeartbeat(d time.Duration) {
	if d > 0 {
		s.streamHeartbeat = d
	}
}

// SetReadOnly makes the server's write routes refuse with 405 instead of
// running: reads stay public, and the automatic refresher outside the API
// keeps the registry current. Call it before Routes mounts the routes,
// where the choice between a real handler and a refusal is made once. It
// mirrors SetLastSweep, which serve also sets at startup.
func (s *Server) SetReadOnly(readOnly bool) {
	s.readOnly = readOnly
}

// SetAPITokens requires write routes to present one of the given bearer
// tokens. Call it before Routes mounts, where the gate is built once.
// An empty list keeps every write public; values are compared in constant
// time, and nothing logs or echoes them, matching how the config keeps
// them out of trouble.
func (s *Server) SetAPITokens(tokens []string) {
	s.tokens = tokens
}

// SetLastSweep makes GET /api/stats report the most recent automatic
// refresh sweep. Serve calls it when refresh is enabled; without it the
// field is omitted, which is the honest answer when nothing sweeps.
func (s *Server) SetLastSweep(fn func() (refresher.SweepRecord, bool)) {
	s.lastSweep = fn
}

// SetSigner makes the server attach a detached Ed25519 signature to every
// interface response, and publish the verification keys at GET /api/keys.
// retired are earlier public keys that stay published so signatures made
// before a rotation remain verifiable.
func (s *Server) SetSigner(signer *attest.Signer, retired ...ed25519.PublicKey) {
	s.signer = signer
	s.keys = nil
	if signer == nil {
		return
	}
	s.keys = append(s.keys, attest.NewKey(signer.PublicKey(), true))
	for _, pub := range retired {
		if key := attest.NewKey(pub, false); key.ID != s.keys[0].ID {
			s.keys = append(s.keys, key)
		}
	}
}

// Routes returns the API's routes, to be mounted under /api.
func (s *Server) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(compressResponses("application/json", "text/plain"))

	r.Get("/contracts", s.listContracts)
	// POST /decode stays available in read-only mode: it answers with the
	// decoded module and stores nothing, so it is a read with POST form.
	r.Post("/decode", s.rateLimited(s.decodeWasm))
	r.Post("/contracts", s.write(s.rateLimited(s.registerContract)))
	r.Post("/contracts:batch", s.write(s.batchRegisterContracts))
	r.Get("/wasm/{hash}.wasm", s.getWasmModule)
	r.Get("/wasm/{hash}", s.getWasmSpec)
	r.Post("/wasm/{hash}", s.write(s.catalogWasm))
	r.Get("/wasm/{hash}/contracts", s.listWasmContracts)
	r.Get("/contracts/{id}", cacheable(s.getContract))
	r.Put("/contracts/{id}/source", s.write(s.updateContractSource))
	r.Post("/contracts/{id}/verify", s.write(s.verifyPublisher))
	r.Put("/contracts/{id}/deprecation", s.write(s.setContractDeprecation))
	r.Delete("/contracts/{id}/deprecation", s.write(s.clearContractDeprecation))
	r.Post("/contracts/{id}/refresh", s.write(s.rateLimited(s.refreshContract)))
	r.Get("/contracts/{id}/versions", s.listVersions)
	r.Get("/contracts/{id}/functions", s.listFunctions)
	r.Get("/contracts/{id}/events", s.listEvents)
	r.Get("/contracts/{id}/functions/{fn}", cacheable(s.getFunction))
	r.Get("/contracts/{id}/functions/{fn}/schema.json", s.getFunctionSchema)
	r.Get("/contracts/{id}/functions/{fn}/example.json", s.getFunctionExample)
	r.Get("/contracts/{id}/client.ts", cacheable(s.getTypeScriptClient))
	r.Get("/contracts/{id}/client.go", cacheable(s.getGoClient))
	r.Get("/contracts/{id}/client.py", cacheable(s.getPythonClient))
	r.Get("/keys", s.getKeys)
	r.Get("/stats", s.getStats)
	r.Get("/upgrades", s.listUpgrades)
	r.Get("/upgrades/stream", s.streamUpgrades)
	r.Get("/upgrades.atom", s.upgradesAtom)
	r.Get("/openapi.json", serveOpenAPI)
	r.Post("/subscriptions", s.write(s.subscribeContract))
	r.Get("/subscriptions", s.listSubscriptions)
	r.Delete("/subscriptions/{secret}", s.write(s.unsubscribeContract))

	return r
}

// write chooses at mount time what a write route does: its real handler in
// normal operation, or a 405 refusal in read-only mode. The routes stay
// mounted either way, so a writer hitting a read-only mirror is told the
// method is refused rather than sent a 404 that reads as an unknown
// resource.
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc {
	if s.readOnly {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusMethodNotAllowed,
				"this SoroVault is running read-only; write access is disabled")
		}
	}
	if len(s.tokens) > 0 {
		return s.requireBearer(h)
	}
	return h
}

// requireBearer serves the wrapped route only when the request presents
// one of the configured tokens as `Authorization: Bearer <token>`.
//
// Malformed credentials and wrong tokens get different status codes — a
// malformed header is a client mistake worth teaching, while a valid
// header with the wrong secret is an access attempt and only ever earns
// "unauthorized", which carries the presented secret nowhere.
func (s *Server) requireBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeError(w, http.StatusBadRequest,
				"Authorization header must be Bearer authentication")
			return
		}
		if !s.tokenMatches(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sorovault"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// tokenMatches compares a presented secret against every configured token
// regardless of any match: the loop runs whole, so success is not told
// apart from failure by how early it stopped. Every comparison is
// constant time; lengths differ, subtleties and all, so timing leaks
// nothing more than the response does.
func (s *Server) tokenMatches(presented string) bool {
	matches := 0
	for _, token := range s.tokens {
		matches |= subtle.ConstantTimeCompare([]byte(presented), []byte(token))
	}
	return matches == 1
}

var commitHash = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// parseWasmHash normalises a WASM hash from the URL.
//
// A hash is the SHA-256 of the module, so it is 32 bytes of hexadecimal;
// the hex is lower-cased because that is how the registry stores it.
func parseWasmHash(raw string) (string, error) {
	rawHash, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(rawHash) != sha256.Size {
		return "", fmt.Errorf("%w: %q", registry.ErrInvalidWasmHash, raw)
	}
	return hex.EncodeToString(rawHash), nil
}

func (s *Server) getWasmSpec(w http.ResponseWriter, r *http.Request) {
	wasmHash, err := parseWasmHash(chi.URLParam(r, "hash"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wasm hash")
		return
	}
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stored, err := s.reg.Store().GetWasmSpec(r.Context(), network, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if s.signer != nil {
		body, err := json.Marshal(stored.Interface)
		if err != nil {
			s.fail(w, r, fmt.Errorf("api: encoding interface for wasm %s: %w", wasmHash, err))
			return
		}
		s.attest(w, r, attest.Statement{
			Network: stored.Network, WasmHash: stored.WasmHash, Interface: body,
		})
	}
	writeJSON(w, http.StatusOK, stored)
}

// getWasmModule serves GET /api/wasm/{hash}.wasm: the uploaded module
// itself, fetched from the configured network and verified against the hash
// in the URL. Because the URL is content-addressed, the response is
// immutable and safe to cache for as long as a client likes.
//
// The bytes are not stored by SoroVault — the chain remains the source of
// truth — so ?network, when given, must name the network this instance is
// pointed at. Serving another network's bytes under this hash would defeat
// the verification the hash is meant to provide.
func (s *Server) getWasmModule(w http.ResponseWriter, r *http.Request) {
	wasmHash, err := parseWasmHash(chi.URLParam(r, "hash"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wasm hash")
		return
	}

	network, err := s.reg.Network(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if requested := strings.TrimSpace(r.URL.Query().Get("network")); requested != "" && stellar.NetworkName(requested) != network {
		writeError(w, http.StatusBadRequest,
			"this SoroVault fetches modules from the "+stellar.NetworkLogLabel(network)+" network")
		return
	}

	module, err := s.reg.Wasm(r.Context(), wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/wasm")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(module)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(module); err != nil {
		s.log.DebugContext(r.Context(), "writing wasm module", "wasm_hash", wasmHash, "error", err)
	}
}

// listWasmContracts serves GET /api/wasm/{hash}/contracts.
//
// It answers "who else runs this code?", which is the question a
// vulnerability in a shared WASM raises: every instance of a token
// template, every vault a factory deployed. ?ever=true widens the answer
// to the contracts that ran the code in the past and have since been
// upgraded.
func (s *Server) listWasmContracts(w http.ResponseWriter, r *http.Request) {
	wasmHash, err := parseWasmHash(chi.URLParam(r, "hash"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid wasm hash")
		return
	}

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	q := r.URL.Query()
	filter := store.WasmFilter{
		Network:  network,
		WasmHash: wasmHash,
		Ever:     boolParam(q.Get("ever")),
		Limit:    intParam(q.Get("limit"), 0),
		Offset:   intParam(q.Get("offset"), 0),
	}
	filter.Normalize()

	page, err := s.reg.Store().ListContractsByWasm(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, wasmContractsResponse{
		WasmHash:  wasmHash,
		Network:   network,
		Ever:      filter.Ever,
		Contracts: page.Contracts,
		Total:     page.Total,
		Limit:     filter.Limit,
		Offset:    filter.Offset,
	})
}

func (s *Server) catalogWasm(w http.ResponseWriter, r *http.Request) {
	result, err := s.reg.CatalogWasm(r.Context(), chi.URLParam(r, "hash"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// maxDecodeBody caps an uploaded module at 512 KiB — four times the 128 KiB
// the network allows as contract code, so any module that could ever run on
// chain fits. It stays deliberately small because this is the only endpoint
// that reads an arbitrary blob, and each concurrent upload is held in full:
// until per-client rate limiting exists, the body limit is what bounds
// memory under a flood of requests.
const maxDecodeBody = 512 << 10

// errDecodeTooLarge reports an upload beyond maxDecodeBody, so the handler
// can answer 413 instead of the 400 every other malformed body earns.
var errDecodeTooLarge = fmt.Errorf("request body exceeds the %d-byte limit", maxDecodeBody)

// decodeWasm serves POST /api/decode: it decodes an uploaded WASM module
// and returns its interface, without storing anything. The endpoint touches
// neither the network nor the database, and reuses spec.XDRDecoder — the
// same decoder every other path goes through, so its limits apply here
// unchanged.
//
// The body is either the raw module, whatever the content type, or a
// multipart/form-data upload whose first file part carries it. ?format=ts
// answers with the TypeScript client for the interface instead of the ABI
// JSON.
func (s *Server) decodeWasm(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "ts" {
		writeError(w, http.StatusBadRequest, "format must be \"json\" or \"ts\"")
		return
	}

	module, err := readWasmBody(r, maxDecodeBody)
	switch {
	case errors.Is(err, errDecodeTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Every decoder failure is a property of the upload — an unparseable
	// module, a missing spec section, an entry stream that does not
	// convert — never a server fault, so none of it becomes a 500.
	var decoder spec.XDRDecoder
	iface, err := decoder.Decode(module)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if format == "ts" {
		// There is no contract ID or network yet — the module is not
		// deployed — so only the hash, derived from the bytes at hand,
		// identifies the interface version the client was built from.
		sum := sha256.Sum256(module)
		source, err := codegen.TypeScript(iface, codegen.Source{
			WasmHash: hex.EncodeToString(sum[:]),
		})
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(source))
		return
	}

	writeJSON(w, http.StatusOK, iface)
}

// readWasmBody reads the uploaded module from r: a raw body, or the first
// file part of a multipart/form-data upload. It rejects an empty body and
// anything larger than limit, which is enforced as the bytes arrive, so an
// oversized upload is never held in full.
func readWasmBody(r *http.Request, limit int) ([]byte, error) {
	var body io.Reader = r.Body

	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mediaType == "multipart/form-data" {
		file, err := multipartWasmFile(r)
		if err != nil {
			return nil, err
		}
		body = file
	}

	module, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	if len(module) > limit {
		return nil, errDecodeTooLarge
	}
	if len(module) == 0 {
		return nil, errors.New("request body is empty")
	}
	return module, nil
}

// multipartWasmFile returns the first file part of a multipart upload — the
// part a browser or curl calls a file. Plain fields are skipped, so a form
// carrying no file is refused rather than decoded as if its field names
// were a WASM module.
func multipartWasmFile(r *http.Request) (io.Reader, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("invalid multipart body: %w", err)
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("multipart body contains no file part")
		}
		if err != nil {
			return nil, fmt.Errorf("invalid multipart body: %w", err)
		}
		if part.FileName() != "" {
			return part, nil
		}
	}
}

func (s *Server) updateContractSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceURL := strings.TrimSpace(req.SourceURL)
	parsed, err := url.Parse(sourceURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "source_url must be an absolute HTTP or HTTPS repository URL")
		return
	}
	commit := strings.TrimSpace(req.SourceCommit)
	if commit != "" && !commitHash.MatchString(commit) {
		writeError(w, http.StatusBadRequest, "source_commit must be a 40- or 64-character hexadecimal commit hash")
		return
	}
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	contractID := chi.URLParam(r, "id")
	if err := s.reg.Store().UpdateSource(r.Context(), network, contractID, sourceURL, commit); err != nil {
		s.fail(w, r, err)
		return
	}
	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

// attest attaches a detached signature over the statement to the response.
//
// A signing failure leaves the response unsigned and logs it: the data is
// still right, and a client that requires a signature will notice it is
// missing and refuse the response, which is what the signature is for.
func (s *Server) attest(w http.ResponseWriter, r *http.Request, st attest.Statement) {
	if s.signer == nil {
		return
	}
	sig, err := s.signer.Sign(st)
	if err != nil {
		s.log.ErrorContext(r.Context(), "signing interface response failed")
		return
	}
	w.Header().Set(attest.SignatureHeader, sig)
	w.Header().Set(attest.KeyIDHeader, s.signer.KeyID())
}

// getKeys serves GET /api/keys: the public keys interface signatures verify
// against. The list is empty when the registry does not sign.
func (s *Server) getKeys(w http.ResponseWriter, _ *http.Request) {
	keys := s.keys
	if keys == nil {
		keys = []attest.Key{}
	}
	writeJSON(w, http.StatusOK, keysResponse{Keys: keys})
}

// verifyPublisher serves POST /api/contracts/{id}/verify.
//
// The body is a claim signed by the account that deployed the contract; see
// registry.VerifyPublisher for what makes one acceptable. Nothing else
// authenticates the caller, because the signature is the authentication.
func (s *Server) verifyPublisher(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Claim.ContractID != chi.URLParam(r, "id") {
		writeError(w, http.StatusBadRequest, "claim.contract_id does not match the contract in the URL")
		return
	}

	contract, err := s.reg.VerifyPublisher(r.Context(), registry.PublisherClaim{
		Claim:     req.Claim,
		Signer:    strings.TrimSpace(req.Signer),
		Signature: strings.TrimSpace(req.Signature),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

// deprecationRequest is the body of PUT /contracts/{id}/deprecation. Both
// fields are optional: a contract can be retired without a recorded
// destination.
type deprecationRequest struct {
	Successor string `json:"successor,omitempty"`
	Note      string `json:"note,omitempty"`
}

// setContractDeprecation serves PUT
// /api/contracts/{id}/deprecation: mark a contract deprecated, with an
// optional successor and note. It answers with the updated contract, the
// same shape the source endpoint answers with.
func (s *Server) setContractDeprecation(w http.ResponseWriter, r *http.Request) {
	var req deprecationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	contractID := chi.URLParam(r, "id")
	successor := strings.TrimSpace(req.Successor)
	if successor == contractID {
		writeError(w, http.StatusBadRequest,
			"the successor must be a contract other than the one being deprecated")
		return
	}
	if successor != "" {
		if _, err := s.reg.Store().GetContract(r.Context(), network, successor); err != nil {
			writeError(w, http.StatusBadRequest, "successor "+successor+" is not registered on "+network)
			return
		}
	}
	if err := s.reg.Store().SetDeprecation(r.Context(), network, contractID, successor,
		strings.TrimSpace(req.Note)); err != nil {
		s.fail(w, r, err)
		return
	}
	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

// clearContractDeprecation serves DELETE
// /api/contracts/{id}/deprecation: lift the mark, restoring the contract
// to active use.
func (s *Server) clearContractDeprecation(w http.ResponseWriter, r *http.Request) {
	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	contractID := chi.URLParam(r, "id")
	if err := s.reg.Store().ClearDeprecation(r.Context(), network, contractID); err != nil {
		s.fail(w, r, err)
		return
	}
	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

// openAPISpec describes every route above. TestOpenAPICoversEveryRoute
// fails if a route is added without documenting it, or documented without
// existing.
//
//go:embed openapi.json
var openAPISpec []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

// getTypeScriptClient serves GET /api/contracts/{id}/client.ts: a typed
// TypeScript client for the contract's current interface, or for an older
// one with ?wasm_hash=. ?download=1 asks the browser to save it.
func (s *Server) getTypeScriptClient(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	var deprecation *codegen.Deprecation
	if contract, err := s.reg.Store().GetContract(r.Context(), network, contractID); err == nil && contract.DeprecatedSince != nil {
		// The client is generated from the interface, but the mark is
		// on the contract: a generated file exported today should warn
		// its importer about a migration that happened last week.
		deprecation = &codegen.Deprecation{
			Since:     contract.DeprecatedSince.UTC().Format(time.RFC3339),
			Successor: contract.Successor,
			Note:      contract.DeprecationNote,
		}
	}

	source, err := codegen.TypeScript(stored.Interface, codegen.Source{
		ContractID:  stored.ContractID,
		Network:     stored.Network,
		WasmHash:    stored.WasmHash,
		Deprecation: deprecation,
		WithZod:     r.URL.Query().Get("zod") != "",
	})
	if err != nil {
		// The interface decoded, but cannot be expressed as TypeScript —
		// a property of the contract, not a server fault.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+contractID+`.client.ts"`)
	}
	_, _ = w.Write([]byte(source)) //nolint:gosec // generated TypeScript, served as text/plain with nosniff
}

// getGoClient serves GET /api/contracts/{id}/client.go: a typed Go client
// for the contract's current interface, or an older one with ?wasm_hash=.
// ?pkg= names the generated package (default "contract"); ?download=1 asks
// the browser to save it.
func (s *Server) getGoClient(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	source, err := codegen.Go(stored.Interface, codegen.Source{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
	}, strings.TrimSpace(r.URL.Query().Get("pkg")))
	if err != nil {
		// The interface decoded, but cannot be expressed as Go —
		// a property of the contract, not a server fault.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+contractID+`.client.go"`)
	}
	_, _ = w.Write([]byte(source)) //nolint:gosec // generated Go, served as text/plain with nosniff
}

// getPythonClient serves GET /api/contracts/{id}/client.py: a typed Python
// client for the contract's current interface, or an older one with
// ?wasm_hash=. ?download=1 asks the browser to save it.
func (s *Server) getPythonClient(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	source, err := codegen.Python(stored.Interface, codegen.Source{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
	})
	if err != nil {
		// The interface decoded, but cannot be expressed as Python —
		// a property of the contract, not a server fault.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+contractID+`.client.py"`)
	}
	_, _ = w.Write([]byte(source)) //nolint:gosec // generated Python, served as text/plain with nosniff
}

// cacheLifetime is how long a response pinned by ?wasm_hash= may be reused
// without asking again. A year is the practical maximum for a public
// resource; the version behind such a response cannot change, because the
// hash is the SHA-256 of the module that produced it.
const cacheLifetime = 31536000 // one year, in seconds

// cacheable gives a handler's 200 responses an ETag and the Cache-Control
// that matches how stable they are, and answers a matching If-None-Match
// with 304 Not Modified and no body.
//
// The ETag is a hash of the encoded body rather than of the WASM hash alone,
// because the body also carries what SoroVault itself contributes: the ABI
// version it decoded with, the generator behind client.ts, and the source
// fields a later edit can change. Hashing the body means every one of those
// invalidates the cached copy on its own, with no version constant for a
// future change to be forgotten.
//
// The stability comes from the request. ?wasm_hash= names a version, and a
// version is immutable, so the answer may sit in a cache for a year. Without
// it the response describes "current", which moves when the contract is
// upgraded, so the client must revalidate: no-cache means the cached body is
// never used without a round trip, but a 304 still spares the transfer.
//
// Anything that is not a 200 passes straight through. An error body names a
// problem that may be gone on the next request, and must not be remembered.
func cacheable(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buffer := newResponseBuffer()
		next(buffer, r)

		if buffer.status != http.StatusOK {
			buffer.writeTo(w)
			return
		}

		sum := sha256.Sum256(buffer.body.Bytes())
		etag := `"` + hex.EncodeToString(sum[:]) + `"`

		cacheControl := "no-cache" // "current" moves; revalidate before use
		if strings.TrimSpace(r.URL.Query().Get("wasm_hash")) != "" {
			cacheControl = fmt.Sprintf("public, max-age=%d, immutable", cacheLifetime)
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", cacheControl)

		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		buffer.writeTo(w)
	}
}

// etagMatches reports whether an If-None-Match header names etag. The header
// carries a comma-separated list, may wrap each tag in W/ for a weak
// comparison, and may be * to match anything; If-None-Match compares
// weakly, so a W/ prefix does not stop a match.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// minCompressBytes is the size below which the extra encoding work and
// headers cost more than they save. Health checks, small errors and tiny
// listings stay plain.
const minCompressBytes = 1024

// compressResponses gzips large responses of the listed content types when
// the client accepts gzip.
//
// It buffers the response before deciding, because chi's compressor decides
// at WriteHeader time while the size is only known after the body is written.
// API responses are bounded and already materialised in memory, so the delay
// is not visible; the win is that a small response never carries a
// Content-Encoding it did not need.
func compressResponses(types ...string) func(http.Handler) http.Handler {
	compressor := middleware.NewCompressor(5, types...)
	compressible := make(map[string]struct{}, len(types))
	for _, contentType := range types {
		compressible[contentType] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
				next.ServeHTTP(w, r)
				return
			}

			buffer := newResponseBuffer()
			buffer.dst = w
			var writer http.ResponseWriter = buffer
			if flusher, ok := w.(http.Flusher); ok {
				writer = &flushResponseBuffer{responseBuffer: buffer, flusher: flusher}
			}
			next.ServeHTTP(writer, r)

			if buffer.streamed {
				return
			}

			contentType, _, _ := strings.Cut(buffer.Header().Get("Content-Type"), ";")
			_, allowed := compressible[contentType]
			if contentType == "text/event-stream" || buffer.body.Len() < minCompressBytes || !allowed ||
				buffer.Header().Get("Content-Encoding") != "" {
				buffer.writeTo(w)
				return
			}

			compressor.Handler(http.HandlerFunc(func(cw http.ResponseWriter, _ *http.Request) {
				copyHeader(cw.Header(), buffer.Header())
				cw.WriteHeader(buffer.status)
				_, _ = cw.Write(buffer.body.Bytes())
			})).ServeHTTP(w, r)
		})
	}
}

// acceptsGzip reports whether the header allows gzip, honouring an explicit
// q=0 refusal.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, parameter := range fields[1:] {
			name, value, ok := strings.Cut(parameter, "=")
			if ok && strings.EqualFold(strings.TrimSpace(name), "q") &&
				strings.TrimSpace(value) == "0" {
				return false
			}
		}
		return true
	}
	return false
}

// responseBuffer records a response so compression can be decided once its
// full size is known. When Flush is called, it switches to streaming directly
// to dst so Server-Sent Events are never buffered or compressed.
type responseBuffer struct {
	dst         http.ResponseWriter
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool
	streamed    bool
}

type flushResponseBuffer struct {
	*responseBuffer
	flusher http.Flusher
}

func (b *flushResponseBuffer) Flush() {
	b.flushToUnderlying()
	b.flusher.Flush()
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{header: make(http.Header), status: http.StatusOK}
}

func (b *responseBuffer) Header() http.Header { return b.header }

func (b *responseBuffer) WriteHeader(status int) {
	if b.wroteHeader {
		return
	}
	b.wroteHeader = true
	b.status = status
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	if b.streamed && b.dst != nil {
		return b.dst.Write(p)
	}
	return b.body.Write(p)
}

func (b *responseBuffer) flushToUnderlying() {
	if b.dst == nil {
		return
	}
	if !b.streamed {
		b.streamed = true
		copyHeader(b.dst.Header(), b.header)
		b.dst.WriteHeader(b.status)
	}
	if b.body.Len() > 0 {
		_, _ = b.dst.Write(b.body.Bytes())
		b.body.Reset()
	}
}

func (b *responseBuffer) writeTo(w http.ResponseWriter) {
	copyHeader(w.Header(), b.header)
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

// Middleware returns the stack the whole server runs behind: a request ID, a
// panic guard, and a bound on how long any one request may take.
//
// There is deliberately no RealIP middleware: it trusts X-Forwarded-For from
// any client, and nothing here needs the client address.
func Middleware(log *slog.Logger) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		middleware.RequestID,
		requestIDHeader,
		middleware.Recoverer,
		requestLogger(log),
	}
}

// requestIDHeader copies the request ID onto the response, so a client can
// quote it when reporting a problem. The ID itself is assigned by
// middleware.RequestID.
func requestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(middleware.RequestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}

// getStats serves GET /api/stats.
func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.reg.Store().Stats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	resp := statsResponse{
		Networks:       stats.Networks,
		Contracts:      stats.Contracts,
		Versions:       stats.Versions,
		LatestContract: stats.Latest,
		ReadOnly:       s.readOnly,
	}
	if s.lastSweep != nil {
		if record, ok := s.lastSweep(); ok {
			resp.LastRefresh = &record
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// upgradeFilter reads the paging and network parameters shared by the JSON
// and Atom forms of the upgrade feed.
func upgradeFilter(r *http.Request) store.UpgradeFilter {
	filter := store.UpgradeFilter{
		Network: r.URL.Query().Get("network"),
		Limit:   intParam(r.URL.Query().Get("limit"), 0),
		Offset:  intParam(r.URL.Query().Get("offset"), 0),
	}
	filter.Normalize()
	return filter
}

// listUpgrades serves GET /api/upgrades.
func (s *Server) listUpgrades(w http.ResponseWriter, r *http.Request) {
	filter := upgradeFilter(r)

	page, err := s.reg.Store().ListUpgrades(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, upgradeListResponse{
		Upgrades: page.Upgrades,
		Total:    page.Total,
		Limit:    filter.Limit,
		Offset:   filter.Offset,
	})
}

// UpgradeEventID returns the stable Server-Sent Events identifier for an
// interface upgrade. Passing it back in Last-Event-ID on reconnect replays
// every upgrade observed since.
func UpgradeEventID(u store.Upgrade) string {
	return fmt.Sprintf("urn:sorovault:upgrade:%s:%s:%s", u.Network, u.ContractID, u.NewWasmHash)
}

func matchesUpgradeEventID(u store.Upgrade, lastEventID string) bool {
	switch lastEventID {
	case UpgradeEventID(u),
		fmt.Sprintf("%s:%s:%s", u.Network, u.ContractID, u.NewWasmHash),
		fmt.Sprintf("%s:%s", u.ContractID, u.NewWasmHash),
		u.NewWasmHash,
		u.ObservedAt.UTC().Format(time.RFC3339Nano),
		u.ObservedAt.UTC().Format(time.RFC3339):
		return true
	default:
		return false
	}
}

// streamUpgrades serves GET /api/upgrades/stream as Server-Sent Events.
func (s *Server) streamUpgrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	network := strings.TrimSpace(q.Get("network"))
	contractID := strings.TrimSpace(q.Get("contract_id"))
	if contractID != "" {
		if err := stellar.ValidateContractID(contractID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	filter := refresher.StreamFilter{
		Network:    network,
		ContractID: contractID,
	}
	sub, err := s.broker.Subscribe(filter)
	if err != nil {
		if errors.Is(err, refresher.ErrTooManySubscribers) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		s.fail(w, r, err)
		return
	}
	defer sub.Close()

	var missed []store.Upgrade
	lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if lastEventID != "" {
		missed, err = s.missedUpgrades(r, filter, lastEventID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	replayed := make(map[string]struct{}, len(missed))
	for _, u := range missed {
		id := UpgradeEventID(u)
		replayed[id] = struct{}{}
		if !writeUpgradeSSE(w, u) {
			return
		}
	}
	flusher.Flush()

	heartbeat := s.streamHeartbeat
	if heartbeat <= 0 {
		heartbeat = defaultStreamHeartbeat
	}
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case u := <-sub.C:
			id := UpgradeEventID(u)
			if _, seen := replayed[id]; seen {
				delete(replayed, id)
				continue
			}
			if !writeUpgradeSSE(w, u) {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) missedUpgrades(r *http.Request, filter refresher.StreamFilter, lastEventID string) ([]store.Upgrade, error) {
	const maxReplayPages = 10
	var collected []store.Upgrade
	found := false

	for pageIdx := range maxReplayPages {
		page, err := s.reg.Store().ListUpgrades(r.Context(), store.UpgradeFilter{
			Network: filter.Network,
			Limit:   store.MaxLimit,
			Offset:  pageIdx * store.MaxLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, u := range page.Upgrades {
			if matchesUpgradeEventID(u, lastEventID) {
				found = true
				break
			}
			if filter.Matches(u) {
				collected = append(collected, u)
			}
		}
		if found || len(page.Upgrades) == 0 || (pageIdx+1)*store.MaxLimit >= page.Total {
			break
		}
	}

	if !found {
		return nil, nil
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, nil
}

func writeUpgradeSSE(w http.ResponseWriter, u store.Upgrade) bool {
	data, err := json.Marshal(u)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: upgrade\ndata: %s\n\n", UpgradeEventID(u), data)
	return err == nil
}

func (s *Server) publishUpgrade(result *registry.Result) {
	if s.broker == nil || result == nil || !result.Changed || result.Created {
		return
	}
	observedAt := result.Contract.LastRefreshed.UTC()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	s.broker.Publish(store.Upgrade{
		ContractID:  result.Contract.ContractID,
		Network:     result.Contract.Network,
		OldWasmHash: result.PreviousWasmHash,
		NewWasmHash: result.Contract.CurrentWasmHash,
		ObservedAt:  observedAt,
	})
}

// listContracts serves GET /api/contracts.
//
// Paging is either by cursor or by the deprecated offset. A cursor names the
// last row of the previous page, so rows added while a client pages through
// the listing are seen exactly once — which is the property offsets lose.
func (s *Server) listContracts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	cursor := q.Get("cursor")
	offset := intParam(q.Get("offset"), 0)
	if cursor != "" && offset != 0 {
		writeError(w, http.StatusBadRequest,
			"give a cursor or an offset, not both; offset is deprecated, so prefer the cursor")
		return
	}

	filter := store.ListFilter{
		Network: q.Get("network"),
		Query:   q.Get("q"),
		// The exact-name filters are repeatable, and every one of them has
		// to match.
		Functions:  q["function"],
		Types:      q["type"],
		Implements: q["implements"],
		Limit:      intParam(q.Get("limit"), 0),
		Offset:     offset,
		Cursor:     cursor,
	}
	filter.Normalize()

	page, err := s.reg.Store().ListContracts(r.Context(), filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, listResponse{
		Contracts: page.Contracts,
		Total:     page.Total,
		Limit:     filter.Limit,
		Offset:    filter.Offset,
		Next:      page.Next,
	})
}

// getContract serves GET /api/contracts/{id}, returning the decoded ABI.
//
// The optional wasm_hash parameter selects a superseded version; without it
// the contract's current interface is returned. ?download=1 asks the browser
// to save the document under a name of its own rather than display it.
func (s *Server) getContract(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	contract, err := s.reg.Store().GetContract(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	cacheHash := wasmHash
	if cacheHash == "" {
		cacheHash = contract.CurrentWasmHash
	}
	key := specCacheKey{network: network, contractID: contractID, wasmHash: cacheHash}
	cached, ok := s.cache.get(key)
	if !ok {
		stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, cacheHash)
		if err != nil {
			s.fail(w, r, err)
			return
		}

		body, err := json.Marshal(stored.Interface)
		if err != nil {
			s.fail(w, r, fmt.Errorf("api: encoding interface for %s: %w", contractID, err))
			return
		}
		cached = cachedInterface{body: body, decodedAt: stored.DecodedAt.UTC()}
		s.cache.add(key, cached)
	}

	// The header has to be set before the body is written.
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+ABIFilename(contractID, cacheHash)+`"`)
	}

	builtin := cacheHash == stellar.BuiltinSACIdentifier
	responseHash := cacheHash
	interfaceID := ""
	if builtin {
		responseHash = ""
		interfaceID = stellar.BuiltinSACIdentifier
	}
	s.attest(w, r, attest.Statement{
		Network: contract.Network, ContractID: contract.ContractID,
		WasmHash: cacheHash, Interface: cached.body,
	})
	writeJSON(w, http.StatusOK, contractResponse{
		ContractID:   contract.ContractID,
		Network:      contract.Network,
		WasmHash:     responseHash,
		InterfaceID:  interfaceID,
		Builtin:      builtin,
		Name:         contract.Name,
		Implements:   contract.Implements,
		SourceURL:    contract.SourceURL,
		SourceCommit: contract.SourceCommit,
		FirstSeen:    contract.FirstSeen.UTC(),
		DecodedAt:    cached.decodedAt,
		MissingSince: contract.MissingSince,
		Origin:       originOf(*contract),
		Verified:     verifiedOf(*contract),
		Current:      cacheHash == contract.CurrentWasmHash,
		Interface:    json.RawMessage(cached.body),
	})
}

// ABIFilename names the file a downloaded interface is saved as: the contract
// it belongs to, and the short form of the WASM hash it was decoded from, so
// two versions of one contract do not collide.
//
// The browse page names the file the same way, so the two cannot disagree
// about what a download will be called.
func ABIFilename(contractID, wasmHash string) string {
	return contractID + "." + shortHash(wasmHash) + ".abi.json"
}

// shortHash is the head of a WASM hash: enough to tell two versions of a
// contract apart, short enough for a filename.
func shortHash(hash string) string {
	const n = 12
	if len(hash) <= n {
		return hash
	}
	return hash[:n]
}

// getFunction serves GET /api/contracts/{id}/functions/{fn}.
func (s *Server) getFunction(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")
	fnName := chi.URLParam(r, "fn")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	fn, ok := stored.Interface.Function(fnName)
	if !ok {
		writeError(w, http.StatusNotFound,
			"no function "+strconv.Quote(fnName)+" in contract "+contractID)
		return
	}

	wasmHash, interfaceID := stored.WasmHash, ""
	if stored.Interface.Builtin {
		wasmHash = ""
		interfaceID = stored.WasmHash
	}
	writeJSON(w, http.StatusOK, functionResponse{
		ContractID:  stored.ContractID,
		Network:     stored.Network,
		WasmHash:    wasmHash,
		InterfaceID: interfaceID,
		Builtin:     stored.Interface.Builtin,
		Signature:   fn.Signature(),
		Function:    fn,
	})
}

// getFunctionSchema serves GET /api/contracts/{id}/functions/{fn}/schema.json:
// a JSON Schema (2020-12) for the function's arguments.
//
// A wallet or a dApp renders a form from the schema and validates what
// the user typed before encoding it, so the schema has to say what each
// argument may be. It is derived from the same stored interface the rest
// of the API serves, and follows the same type conventions as the typed
// client.
func (s *Server) getFunctionSchema(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")
	fnName := chi.URLParam(r, "fn")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	fn, ok := stored.Interface.Function(fnName)
	if !ok {
		writeError(w, http.StatusNotFound,
			"no function "+strconv.Quote(fnName)+" in contract "+contractID)
		return
	}

	schema, err := codegen.JSONSchema(stored.Interface, fn, codegen.Source{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/schema+json")
	_, _ = w.Write(schema) //nolint:gosec // generated JSON Schema, served as application/schema+json
}

// getFunctionExample serves GET
// /api/contracts/{id}/functions/{fn}/example.json: one example argument list
// for the function.
//
// The document is the placeholder argument object the function's JSON Schema
// validates, so a client can take it, fill in the fields it cares about and
// send its first request. It is derived from the same stored interface the
// schema is, which keeps the two from drifting apart.
func (s *Server) getFunctionExample(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")
	fnName := chi.URLParam(r, "fn")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	fn, ok := stored.Interface.Function(fnName)
	if !ok {
		writeError(w, http.StatusNotFound,
			"no function "+strconv.Quote(fnName)+" in contract "+contractID)
		return
	}

	example, err := codegen.Example(stored.Interface, fn, codegen.Source{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(example) //nolint:gosec // generated example, served as application/json
}

// listFunctions serves GET /api/contracts/{id}/functions.
func (s *Server) listFunctions(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	functions := make([]functionSummary, 0, len(stored.Interface.Functions))
	for _, fn := range stored.Interface.Functions {
		functions = append(functions, functionSummary{
			Name:      fn.Name,
			Signature: fn.Signature(),
		})
	}

	wasmHash, interfaceID := stored.WasmHash, ""
	if stored.Interface.Builtin {
		wasmHash = ""
		interfaceID = stored.WasmHash
	}
	writeJSON(w, http.StatusOK, functionListResponse{
		ContractID:  stored.ContractID,
		Network:     stored.Network,
		WasmHash:    wasmHash,
		InterfaceID: interfaceID,
		Builtin:     stored.Interface.Builtin,
		Functions:   functions,
	})
}

// listEvents serves GET /api/contracts/{id}/events: the event definitions a
// contract declares, for the indexers that react to them.
//
// An indexer usually wants only the events and not the whole interface, which
// is one large document. Each event is the definition the interface holds
// itself — its name, topic names, parameters with their types and whether
// each is a topic or data, and the data payload's packing — so values here
// match the ones the interface document serves, `?wasm_hash=` selects a
// superseded version the same way, and the shapes stay in step by
// construction.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	wasmHash := strings.TrimSpace(r.URL.Query().Get("wasm_hash"))
	stored, err := s.reg.Store().GetSpec(r.Context(), network, contractID, wasmHash)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	events := stored.Interface.Events
	if events == nil {
		// A contract with no events still answers with an empty list,
		// the way the interface document answers with [].
		events = []model.Event{}
	}

	writeJSON(w, http.StatusOK, eventListResponse{
		ContractID: stored.ContractID,
		Network:    stored.Network,
		WasmHash:   stored.WasmHash,
		Events:     events,
	})
}

// listVersions serves GET /api/contracts/{id}/versions.
func (s *Server) listVersions(w http.ResponseWriter, r *http.Request) {
	contractID := chi.URLParam(r, "id")

	network, err := s.network(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	versions, err := s.reg.Store().ListVersions(r.Context(), network, contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, versionsResponse{
		ContractID: contractID,
		Network:    network,
		Versions:   versions,
	})
}

// registerContract serves POST /api/contracts.
func (s *Server) registerContract(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	contractID := strings.TrimSpace(req.ContractID)
	if contractID == "" {
		writeError(w, http.StatusBadRequest, "contract_id is required")
		return
	}

	result, err := s.reg.Register(r.Context(), contractID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishUpgrade(result)

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

// batchRegisterContracts serves POST /api/contracts:batch.
//
// Seeding a registry over the API would otherwise take one request per
// contract. The contracts are registered one after another, so the batch
// draws on the registry's concurrency budget once per contract rather than
// all at once, and one failure does not stop the rest: every contract gets
// a result of its own.
func (s *Server) batchRegisterContracts(w http.ResponseWriter, r *http.Request) {
	var req batchRegisterRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.ContractIDs) == 0 {
		writeError(w, http.StatusBadRequest, "contract_ids must not be empty")
		return
	}
	if len(req.ContractIDs) > MaxBatchContracts {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"at most %d contract IDs per batch, got %d", MaxBatchContracts, len(req.ContractIDs)))
		return
	}

	results := make([]batchResult, 0, len(req.ContractIDs))
	for _, contractID := range req.ContractIDs {
		if r.Context().Err() != nil {
			// The client has gone; there is nobody left to answer.
			return
		}
		results = append(results, s.registerOne(r, contractID))
	}

	response := batchResponse{Results: results}
	for _, result := range results {
		switch result.Status {
		case batchStatusCreated:
			response.Created++
		case batchStatusUnchanged:
			response.Unchanged++
		case batchStatusChanged:
			response.Changed++
		default:
			response.Failed++
		}
	}

	writeJSON(w, http.StatusOK, response)
}

// registerOne registers a single contract and turns the outcome into one
// result of a batch.
func (s *Server) registerOne(r *http.Request, contractID string) batchResult {
	contractID = strings.TrimSpace(contractID)
	if contractID == "" {
		return batchResult{
			Status: batchStatusError,
			Code:   http.StatusBadRequest,
			Error:  "contract_id is required",
		}
	}

	result, err := s.reg.Register(r.Context(), contractID)
	if err != nil {
		status, message := s.errorStatus(r, err)
		return batchResult{
			ContractID: contractID,
			Status:     batchStatusError,
			Code:       status,
			Error:      message,
		}
	}
	s.publishUpgrade(result)

	status := batchStatusUnchanged
	switch {
	case result.Created:
		status = batchStatusCreated
	case result.Changed:
		status = batchStatusChanged
	}

	return batchResult{
		ContractID: contractID,
		Status:     status,
		Created:    result.Created,
		Changed:    result.Changed,
		Contract:   &result.Contract,
		Interface:  result.Interface,
	}
}

// refreshContract serves POST /api/contracts/{id}/refresh.
func (s *Server) refreshContract(w http.ResponseWriter, r *http.Request) {
	result, err := s.reg.Refresh(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishUpgrade(result)
	writeJSON(w, http.StatusOK, result)
}

// network resolves which network a read applies to: the explicit query
// parameter if given, otherwise the one this instance is pointed at.
func (s *Server) network(r *http.Request) (string, error) {
	if n := strings.TrimSpace(r.URL.Query().Get("network")); n != "" {
		return n, nil
	}
	return s.reg.Network(r.Context())
}

// subscribeRequest is the request body for creating a subscription.
type subscribeRequest struct {
	ContractIDs []string `json:"contract_ids"`
	URL         string   `json:"url"`
}

// subscribeResponse is the response for subscription operations.
type subscribeResponse struct {
	ID        string `json:"id"`
	Network   string `json:"network"`
	Contracts int    `json:"contracts"`
	Active    bool   `json:"active"`
}

// subscriptionListResponse is the response for listing subscriptions.
type subscriptionListResponse struct {
	Subscriptions []store.SubscriptionListItem `json:"subscriptions"`
}

// subscribeContract serves POST /api/subscriptions.
// Registers a webhook URL for a set of contracts, returning a secret used
// to sign deliveries.
func (s *Server) subscribeContract(w http.ResponseWriter, r *http.Request) {
	var req subscribeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSubscription(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	net, _ := s.network(r)
	secret, err := s.reg.Store().Subscribe(r.Context(), net, req.ContractIDs, req.URL)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, subscribeResponse{
		ID:        secret,
		Network:   net,
		Contracts: len(req.ContractIDs),
		Active:    true,
	})
}

// listSubscriptions serves GET /api/subscriptions.
// Lists all subscriptions.
func (s *Server) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	items, err := s.reg.Store().ListSubscriptions(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// A subscription's ID is the secret that signs its deliveries and
	// deletes it, so a public listing must not reveal it.
	for i := range items {
		items[i].ID = ""
	}

	writeJSON(w, http.StatusOK, subscriptionListResponse{
		Subscriptions: items,
	})
}

// unsubscribeContract serves DELETE /api/subscriptions.
// Removes a subscription by its secret.
func (s *Server) unsubscribeContract(w http.ResponseWriter, r *http.Request) {
	net, _ := s.network(r)
	secret := chi.URLParam(r, "secret")
	if secret == "" {
		writeError(w, http.StatusBadRequest, "secret is required")
		return
	}

	err := s.reg.Store().Unsubscribe(r.Context(), secret)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, subscribeResponse{
		ID:        secret,
		Network:   net,
		Contracts: 0,
		Active:    false,
	})
}

// fail maps a domain error onto a status code and writes it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if ctxErr := r.Context().Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		// The client went away; there is nobody left to read a response.
		return
	}

	status, message := s.errorStatus(r, err)
	if errors.Is(err, registry.ErrBusy) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(s.reg.AcquireTimeout())))
	}
	writeError(w, status, message)
}

// errorStatus maps a domain error onto the status code it deserves and the
// message that may be shown to the caller.
//
// Everything unrecognised becomes a 500 with a generic body, so an internal
// failure cannot leak a connection string or query text to a caller.
func (s *Server) errorStatus(r *http.Request, err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrMalformedCursor):
		// A cursor is a client-typed string; an unreadable one is the
		// caller's mistake, not the server's.
		return http.StatusBadRequest, err.Error()

	case errors.Is(err, store.ErrNotFound), errors.Is(err, stellar.ErrContractNotFound), errors.Is(err, stellar.ErrWasmNotFound):
		return http.StatusNotFound, err.Error()

	case errors.Is(err, stellar.ErrNoWasm), errors.Is(err, spec.ErrNoSpecSection):
		// The request was well-formed and the contract exists; it just has
		// no interface to serve.
		return http.StatusUnprocessableEntity, err.Error()

	case errors.Is(err, spec.ErrLimitExceeded), errors.Is(err, wasm.ErrModuleTooLarge):
		// The module or its spec section is larger or deeper than SoroVault
		// will decode. That is a property of the contract, not a server
		// fault, so it is a 422 with the limit in the message rather than a
		// 500.
		return http.StatusUnprocessableEntity, err.Error()

	case errors.Is(err, registry.ErrNetworkMismatch):
		return http.StatusServiceUnavailable, err.Error()

	case errors.Is(err, registry.ErrBusy):
		return http.StatusServiceUnavailable, err.Error()

	case errors.Is(err, registry.ErrInvalidClaim):
		return http.StatusBadRequest, err.Error()

	case errors.Is(err, registry.ErrNotDeployer):
		return http.StatusForbidden, err.Error()

	case errors.Is(err, registry.ErrStaleClaim), errors.Is(err, registry.ErrDeployerUnknown):
		return http.StatusUnprocessableEntity, err.Error()

	case isBadContractID(err), errors.Is(err, registry.ErrInvalidWasmHash):
		return http.StatusBadRequest, err.Error()

	default:
		s.log.ErrorContext(r.Context(), "request failed")
		return http.StatusInternalServerError, "internal error"
	}
}

// isBadContractID reports whether err came from strkey rejecting an address.
func isBadContractID(err error) bool {
	return strings.Contains(err.Error(), "is not a valid contract address") ||
		strings.Contains(err.Error(), "is not a contract address")
}

// intParam parses a query parameter, falling back on anything unparseable
// rather than rejecting the request over a malformed paging hint.
func intParam(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

// boolParam parses a query parameter as a flag, treating anything
// unparseable as false: the parameter only ever widens an answer, so a
// malformed value must not widen it by accident.
func boolParam(raw string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return v
}

// retryAfterSeconds renders a wait as the whole seconds a client should
// wait before retrying, which is what the Retry-After header asks for.
func retryAfterSeconds(d time.Duration) int {
	seconds := int((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// maxSubscriptionContracts bounds how many contracts one subscription covers.
const maxSubscriptionContracts = 20

// validateSubscription checks a subscription request before anything is
// stored: at least one well-formed contract ID, and an absolute HTTP(S)
// webhook URL.
func validateSubscription(req subscribeRequest) error {
	if len(req.ContractIDs) == 0 {
		return errors.New("contract_ids must name at least one contract")
	}
	if len(req.ContractIDs) > maxSubscriptionContracts {
		return fmt.Errorf("contract_ids may name at most %d contracts", maxSubscriptionContracts)
	}
	for _, id := range req.ContractIDs {
		if err := stellar.ValidateContractID(id); err != nil {
			return err
		}
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an absolute http or https URL")
	}
	return nil
}
