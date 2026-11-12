// Info reads the memory facts of a GGUF model file: the shape of its layers
// (attention, sliding-window, recurrent) and the byte size of its tensors by
// class, so memory can be counted from the file instead of hand-typed
// constants. Only the header is read, never tensor data; the file is not
// trusted, so every count and length is bounded and a malformed header is an
// error. The rules mirror llama.cpp at the pinned build b11541 (sources named
// beside each rule).
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	maxTensors   = 1 << 20
	maxDims      = 4       // GGML_MAX_DIMS
	maxLayers    = 512     // LLAMA_MAX_LAYERS
	maxKeptArray = 1 << 12 // longer arrays (vocab tables) are skipped
	maxKeptStr   = 1 << 12 // longer strings (templates) are skipped
	maxBytes     = 1 << 60

	// DefaultUBatch is llama.cpp's default n_ubatch (common.h). A sliding-
	// window cache holds window + n_ubatch cells.
	DefaultUBatch = 512

	// cellPad is the padding llama.cpp applies to the context and to the
	// sliding-window cache (llama-context.cpp: GGML_PAD(n_ctx, 256);
	// llama-kv-cache-iswa.cpp: size_swa padded to 256).
	cellPad = 256
)

// ModelInfo is what a GGUF header says about a model's memory. Per-layer
// slices have BlockCount entries.
type ModelInfo struct {
	Arch     string `json:"arch"`
	Name     string `json:"name,omitempty"`
	FileType int    `json:"file_type"` // general.file_type (llama_ftype), -1 when absent

	BlockCount      int `json:"block_count"`            // every block, MTP ones included
	NextNLayers     int `json:"nextn_layers,omitempty"` // trailing MTP blocks, outside the main KV cache
	ContextLength   int `json:"context_length"`
	EmbeddingLength int `json:"embedding_length"`

	HeadCount   []int `json:"head_count,omitempty"`
	HeadCountKV []int `json:"head_count_kv,omitempty"`
	KeyLength   int   `json:"key_length"`   // per head, full-attention layers
	ValueLength int   `json:"value_length"` // per head, full-attention layers

	SlidingWindow  int    `json:"sliding_window,omitempty"`
	KeyLengthSWA   int    `json:"key_length_swa,omitempty"`
	ValueLengthSWA int    `json:"value_length_swa,omitempty"`
	SWALayers      []bool `json:"swa_layers,omitempty"`

	FullAttentionInterval int    `json:"full_attention_interval,omitempty"`
	RecurrentLayers       []bool `json:"recurrent_layers,omitempty"`
	KVLayers              []bool `json:"kv_layers,omitempty"` // layers that own a slice of the main KV cache

	SSMConvKernel   int `json:"ssm_conv_kernel,omitempty"`
	SSMStateSize    int `json:"ssm_state_size,omitempty"`
	SSMGroupCount   int `json:"ssm_group_count,omitempty"`
	SSMTimeStepRank int `json:"ssm_time_step_rank,omitempty"`
	SSMInnerSize    int `json:"ssm_inner_size,omitempty"`
	ShortConvCache  int `json:"shortconv_l_cache,omitempty"`

	ExpertCount     int `json:"expert_count,omitempty"`
	ExpertUsedCount int `json:"expert_used_count,omitempty"`

	TensorCount int         `json:"tensor_count"`
	Bytes       TensorBytes `json:"bytes"`

	// LayoutVerified is true when the layer rules for Arch were checked
	// against llama.cpp b11541. Otherwise every layer is given full attention,
	// and a recurrent state when the file has state keys, so the counts are an
	// upper bound; LayoutNote says so.
	LayoutVerified bool   `json:"layout_verified"`
	LayoutNote     string `json:"layout_note,omitempty"`
}

// TensorBytes is the size of the file's tensors by class. The classes
// overlap only with Total.
type TensorBytes struct {
	Total             int64 `json:"total"`
	Experts           int64 `json:"experts"`             // names containing "_exps"
	TokenEmbedding    int64 `json:"token_embedding"`     // token_embd.*
	Output            int64 `json:"output"`              // output.*
	PerLayerEmbedding int64 `json:"per_layer_embedding"` // per_layer_token_embd.* (gemma3n, gemma4)
	NextN             int64 `json:"nextn"`               // MTP blocks and nextn.* tensors
	Projector         int64 `json:"projector"`           // v.*, mm.*, a.* (multimodal projector)
}

// Info reads the metadata and tensor table of the GGUF file at path.
func Info(path string) (*ModelInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := readInfo(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ggmlType is one row of ggml's type_traits: elements per block and bytes per
// block (ggml/src/ggml.c type_traits, block structs in ggml-common.h).
type ggmlType struct {
	name  string
	block int64
	size  int64
}

// ggmlTypes holds every type a GGUF tensor may have at b11541, by its
// ggml_type number (ggml/include/ggml.h). Removed types are absent.
var ggmlTypes = map[uint32]ggmlType{
	0:  {"F32", 1, 4},
	1:  {"F16", 1, 2},
	2:  {"Q4_0", 32, 18},
	3:  {"Q4_1", 32, 20},
	6:  {"Q5_0", 32, 22},
	7:  {"Q5_1", 32, 24},
	8:  {"Q8_0", 32, 34},
	9:  {"Q8_1", 32, 36},
	10: {"Q2_K", 256, 84},
	11: {"Q3_K", 256, 110},
	12: {"Q4_K", 256, 144},
	13: {"Q5_K", 256, 176},
	14: {"Q6_K", 256, 210},
	15: {"Q8_K", 256, 292},
	16: {"IQ2_XXS", 256, 66},
	17: {"IQ2_XS", 256, 74},
	18: {"IQ3_XXS", 256, 98},
	19: {"IQ1_S", 256, 50},
	20: {"IQ4_NL", 32, 18},
	21: {"IQ3_S", 256, 110},
	22: {"IQ2_S", 256, 82},
	23: {"IQ4_XS", 256, 136},
	24: {"I8", 1, 1},
	25: {"I16", 1, 2},
	26: {"I32", 1, 4},
	27: {"I64", 1, 8},
	28: {"F64", 1, 8},
	29: {"IQ1_M", 256, 56},
	30: {"BF16", 1, 2},
	34: {"TQ1_0", 256, 54},
	35: {"TQ2_0", 256, 66},
	39: {"MXFP4", 32, 17},
	40: {"NVFP4", 64, 36},
	41: {"Q1_0", 128, 18},
	42: {"Q2_0", 64, 18},
}

// meta is the metadata kept from a header: integer scalars as int64 (or
// uint64 when too large), floats, bools, short strings, and short integer or
// bool arrays as []int64. Everything else is skipped.
type meta map[string]any

func readInfo(r *bufio.Reader) (*ModelInfo, error) {
	var head [24]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("not a GGUF file: %w", err)
	}
	if string(head[:4]) != "GGUF" {
		return nil, errors.New("not a GGUF file")
	}
	if v := binary.LittleEndian.Uint32(head[4:8]); v < 2 || v > 3 {
		return nil, fmt.Errorf("GGUF version %d is not read here", v)
	}
	nTensors := binary.LittleEndian.Uint64(head[8:16])
	nKeys := binary.LittleEndian.Uint64(head[16:24])
	if nKeys > maxKeys {
		return nil, fmt.Errorf("%d metadata keys is too many", nKeys)
	}
	if nTensors > maxTensors {
		return nil, fmt.Errorf("%d tensors is too many", nTensors)
	}
	md := meta{}
	for i := uint64(0); i < nKeys; i++ {
		key, err := readString(r, maxKeyLen)
		if err != nil {
			return nil, fmt.Errorf("metadata key %d: %w", i, err)
		}
		var kind uint32
		if err := binary.Read(r, binary.LittleEndian, &kind); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		v, err := readValue(r, kind)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if v != nil {
			md[key] = v
		}
	}
	m, err := layout(md)
	if err != nil {
		return nil, err
	}
	m.TensorCount = int(nTensors)
	if err := readTensors(r, nTensors, m); err != nil {
		return nil, err
	}
	return m, nil
}

// readValue reads one metadata value, returning nil for values not kept.
func readValue(r *bufio.Reader, kind uint32) (any, error) {
	switch kind {
	case typeString:
		return readShortString(r)
	case typeArray:
		return readArray(r)
	case typeBool:
		b, err := r.ReadByte()
		return b != 0, err
	case typeFloat32:
		var f float32
		err := binary.Read(r, binary.LittleEndian, &f)
		return float64(f), err
	case typeFloat64:
		var f float64
		err := binary.Read(r, binary.LittleEndian, &f)
		return f, err
	}
	if _, ok := width(kind); !ok {
		return nil, fmt.Errorf("unknown value type %d", kind)
	}
	return readInt(r, kind)
}

// readInt reads an integer of a GGUF integer kind as int64, or as uint64 when
// it does not fit.
func readInt(r *bufio.Reader, kind uint32) (any, error) {
	w, _ := width(kind)
	var b [8]byte
	if _, err := io.ReadFull(r, b[:w]); err != nil {
		return nil, err
	}
	switch kind {
	case typeUint8:
		return int64(b[0]), nil
	case typeInt8:
		return int64(int8(b[0])), nil
	case typeUint16:
		return int64(binary.LittleEndian.Uint16(b[:])), nil
	case typeInt16:
		return int64(int16(binary.LittleEndian.Uint16(b[:]))), nil
	case typeUint32:
		return int64(binary.LittleEndian.Uint32(b[:])), nil
	case typeInt32:
		return int64(int32(binary.LittleEndian.Uint32(b[:]))), nil
	case typeInt64:
		return int64(binary.LittleEndian.Uint64(b[:])), nil
	}
	u := binary.LittleEndian.Uint64(b[:])
	if u > math.MaxInt64 {
		return u, nil
	}
	return int64(u), nil
}

// readShortString keeps a string up to maxKeptStr bytes and skips a longer one.
func readShortString(r *bufio.Reader) (any, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > maxString {
		return nil, fmt.Errorf("a string of %d bytes is too long", n)
	}
	if n > maxKeptStr {
		_, err := io.CopyN(io.Discard, r, int64(n))
		return nil, err
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return string(b), err
}

// readArray keeps a short array of integers or bools as []int64 and skips
// any other array.
func readArray(r *bufio.Reader) (any, error) {
	var elem uint32
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &elem); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	if n > maxArray {
		return nil, fmt.Errorf("an array of %d items is too long", n)
	}
	w, fixed := width(elem)
	isInt := fixed && elem != typeFloat32 && elem != typeFloat64
	if !isInt || n > maxKeptArray {
		if fixed {
			_, err := io.CopyN(io.Discard, r, w*int64(n))
			return nil, err
		}
		if elem != typeString {
			return nil, fmt.Errorf("an array of type %d is not read here", elem)
		}
		for i := uint64(0); i < n; i++ {
			if err := skipString(r); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	out := make([]int64, n)
	for i := range out {
		v, err := readInt(r, elem)
		if err != nil {
			return nil, err
		}
		x, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("array item %d is out of range", i)
		}
		out[i] = x
	}
	return out, nil
}

// int reads an integer key in [0, 2^31). Absent is (0, false, nil); a wrong
// type or range is an error, as llama.cpp refuses such a file.
func (md meta) int(key string) (int, bool, error) {
	v, ok := md[key]
	if !ok {
		return 0, false, nil
	}
	switch x := v.(type) {
	case int64:
		if x >= 0 && x <= math.MaxInt32 {
			return int(x), true, nil
		}
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	}
	return 0, false, fmt.Errorf("%s: not a small non-negative integer", key)
}

// perLayer reads a key that is either one integer for every layer or an array
// of n integers (llama.cpp get_key_or_arr requires exactly n).
func (md meta) perLayer(key string, n int) ([]int, bool, error) {
	v, ok := md[key]
	if !ok {
		return nil, false, nil
	}
	out := make([]int, n)
	if arr, isArr := v.([]int64); isArr {
		if len(arr) != n {
			return nil, false, fmt.Errorf("%s: %d values for %d layers", key, len(arr), n)
		}
		for i, x := range arr {
			if x < 0 || x > math.MaxInt32 {
				return nil, false, fmt.Errorf("%s: value %d out of range", key, x)
			}
			out[i] = int(x)
		}
		return out, true, nil
	}
	x, _, err := md.int(key)
	if err != nil {
		return nil, false, err
	}
	for i := range out {
		out[i] = x
	}
	return out, true, nil
}

// archs whose layer rules were checked against llama.cpp b11541
// (src/models/<arch>.cpp load_arch_hparams, src/llama-model.cpp create_memory).
var verifiedArchs = map[string]bool{
	"llama": true, "qwen2": true, "qwen3": true, "qwen3moe": true, "mistral3": true,
	"gemma2": true, "gemma3": true, "gemma3n": true, "gemma4": true, "gpt-oss": true,
	"qwen3next": true, "qwen35": true, "qwen35moe": true,
	"mamba": true, "mamba2": true, "lfm2": true, "lfm2moe": true,
	"granite-hybrid": true, "jamba": true, "falcon-h1": true,
}

// layout turns metadata into the per-layer shape llama.cpp builds.
func layout(md meta) (*ModelInfo, error) {
	m := &ModelInfo{FileType: -1}
	arch, _ := md["general.architecture"].(string)
	if arch == "" {
		return nil, errors.New("general.architecture is missing")
	}
	m.Arch = arch
	m.Name, _ = md["general.name"].(string)
	if ft, ok, err := md.int("general.file_type"); err != nil {
		return nil, err
	} else if ok {
		m.FileType = ft
	}
	if arch == "clip" {
		// a projector file: tensors only, no layers in the main model.
		m.LayoutVerified = true
		m.LayoutNote = "multimodal projector file"
		return m, nil
	}
	k := func(s string) string { return arch + "." + s }
	var err error
	ints := []struct {
		key string
		dst *int
	}{
		{"block_count", &m.BlockCount},
		{"nextn_predict_layers", &m.NextNLayers},
		{"context_length", &m.ContextLength},
		{"embedding_length", &m.EmbeddingLength},
		{"attention.sliding_window", &m.SlidingWindow},
		{"full_attention_interval", &m.FullAttentionInterval},
		{"ssm.conv_kernel", &m.SSMConvKernel},
		{"ssm.state_size", &m.SSMStateSize},
		{"ssm.group_count", &m.SSMGroupCount},
		{"ssm.time_step_rank", &m.SSMTimeStepRank},
		{"ssm.inner_size", &m.SSMInnerSize},
		{"shortconv.l_cache", &m.ShortConvCache},
		{"expert_count", &m.ExpertCount},
	}
	for _, f := range ints {
		if *f.dst, _, err = md.int(k(f.key)); err != nil {
			return nil, err
		}
	}
	n := m.BlockCount
	if n <= 0 || n > maxLayers {
		return nil, fmt.Errorf("%s: %d blocks is out of range", k("block_count"), n)
	}
	if m.NextNLayers > n {
		return nil, fmt.Errorf("%d MTP blocks of %d", m.NextNLayers, n)
	}
	nTrunk := n - m.NextNLayers // llama-hparams.cpp n_layer()

	if used, ok, err := md.perLayer(k("expert_used_count"), n); err != nil {
		return nil, err
	} else if ok {
		for _, u := range used {
			m.ExpertUsedCount = max(m.ExpertUsedCount, u)
		}
	}

	// llama-model.cpp load_hparams: n_head per layer (default 0), n_head_kv
	// defaults to n_head, head sizes default to n_embd / n_head(0).
	if m.HeadCount, _, err = md.perLayer(k("attention.head_count"), n); err != nil {
		return nil, err
	}
	if m.HeadCount == nil {
		m.HeadCount = make([]int, n)
	}
	m.HeadCountKV = append([]int(nil), m.HeadCount...)
	if kv, ok, err := md.perLayer(k("attention.head_count_kv"), n); err != nil {
		return nil, err
	} else if ok {
		m.HeadCountKV = kv
	}
	if h := m.HeadCount[0]; h > 0 {
		m.KeyLength, m.ValueLength = m.EmbeddingLength/h, m.EmbeddingLength/h
		if v, ok, err := md.int(k("attention.key_length")); err != nil {
			return nil, err
		} else if ok {
			m.KeyLength = v
		}
		if v, ok, err := md.int(k("attention.value_length")); err != nil {
			return nil, err
		} else if ok {
			m.ValueLength = v
		}
	}
	m.KeyLengthSWA, m.ValueLengthSWA = m.KeyLength, m.ValueLength
	if v, ok, err := md.int(k("attention.key_length_swa")); err != nil {
		return nil, err
	} else if ok {
		m.KeyLengthSWA = v
	}
	if v, ok, err := md.int(k("attention.value_length_swa")); err != nil {
		return nil, err
	} else if ok {
		m.ValueLengthSWA = v
	}

	m.SWALayers = make([]bool, n)
	m.RecurrentLayers = make([]bool, n)
	m.KVLayers = make([]bool, n)
	kvFrom := n // layers from kvFrom on keep no KV of their own (has_kv)

	// swaPattern is llama-model.cpp load_swa_pattern: an array wins, else a
	// scalar n_pattern (default def) through llama-hparams.cpp set_swa_pattern.
	// The array may be shorter than the layers (llama-model-loader.cpp get_arr
	// fills from the front); missing layers are not SWA.
	swaKey := k("attention.sliding_window_pattern")
	swaPattern := func(def int, required bool) error {
		if arr, isArr := md[swaKey].([]int64); isArr {
			if len(arr) > maxLayers {
				return fmt.Errorf("%s: %d values", swaKey, len(arr))
			}
			for i := 0; i < len(arr) && i < n; i++ {
				m.SWALayers[i] = arr[i] != 0
			}
			return nil
		}
		if required {
			return fmt.Errorf("%s must be an array", swaKey)
		}
		if v, ok, err := md.int(swaKey); err != nil {
			return err
		} else if ok {
			def = v
		}
		for i := 0; i < nTrunk; i++ {
			m.SWALayers[i] = def == 0 || i%def < def-1
		}
		return nil
	}
	switch arch {
	case "gemma2": // models/gemma2.cpp: window default 4096, pattern 2
		if m.SlidingWindow == 0 {
			m.SlidingWindow = 4096
		}
		err = swaPattern(2, false)
	case "gemma3": // models/gemma3.cpp: SWA only with a window, pattern 6
		if m.SlidingWindow > 0 {
			err = swaPattern(6, false)
		}
	case "gemma3n": // models/gemma3n.cpp: pattern 5, KV only in the first 20 layers
		err = swaPattern(5, false)
		kvFrom = 20
	case "gemma4": // models/gemma4.cpp: pattern array required, shared_kv_layers reuse earlier KV
		err = swaPattern(0, true)
		shared, _, e := md.int(k("attention.shared_kv_layers"))
		if e != nil {
			return nil, e
		}
		if shared > n {
			return nil, fmt.Errorf("%d shared KV layers of %d", shared, n)
		}
		kvFrom = n - shared
	case "gpt-oss": // models/openai-moe.cpp: pattern 2
		err = swaPattern(2, false)
	case "qwen3next", "qwen35", "qwen35moe":
		// models/qwen35.cpp: attention.recurrent_layers, else a layer is
		// recurrent when (i+1) % full_attention_interval != 0 (default 4).
		if rec, ok, e := md.perLayer(k("attention.recurrent_layers"), n); e != nil {
			return nil, e
		} else if ok {
			for i := range rec {
				m.RecurrentLayers[i] = rec[i] != 0
			}
		} else {
			fa := m.FullAttentionInterval
			if _, has := md[k("full_attention_interval")]; !has {
				fa = 4
			}
			if fa <= 0 {
				return nil, fmt.Errorf("%s is %d", k("full_attention_interval"), fa)
			}
			for i := 0; i < n; i++ {
				m.RecurrentLayers[i] = i < nTrunk && (i+1)%fa != 0
			}
		}
	case "mamba", "mamba2": // llm_arch_is_recurrent: every layer
		for i := 0; i < nTrunk; i++ {
			m.RecurrentLayers[i] = true
		}
	case "lfm2", "lfm2moe", "granite-hybrid", "jamba": // recurrent iff n_head_kv == 0
		for i := 0; i < nTrunk; i++ {
			m.RecurrentLayers[i] = m.HeadCountKV[i] == 0
		}
		if arch == "lfm2" && m.SlidingWindow > 0 { // models/lfm2.cpp
			for i := 0; i < nTrunk; i++ {
				m.SWALayers[i] = !m.RecurrentLayers[i]
			}
		}
	case "falcon-h1": // models/falcon-h1.cpp: every layer has both
		for i := 0; i < nTrunk; i++ {
			m.RecurrentLayers[i] = true
		}
	default:
		// Unchecked: an upper bound. Every layer gets full attention (unless
		// the file lists a sliding-window pattern) and, when the file has
		// recurrent-state keys, a recurrent state too.
		if _, isArr := md[swaKey].([]int64); isArr && m.SlidingWindow > 0 {
			err = swaPattern(0, true)
		}
		if m.SSMConvKernel > 0 || m.ShortConvCache > 0 {
			for i := 0; i < nTrunk; i++ {
				m.RecurrentLayers[i] = true
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if arch == "mamba" || arch == "jamba" {
		m.SSMGroupCount = 0 // these loaders never read ssm.group_count
	}

	// Which layers own KV in the main context (llama-model.cpp create_memory,
	// llama-kv-cache.cpp): no MTP block, no recurrent-only layer, none past
	// n_layer_kv_from_start, none without KV heads.
	m.LayoutVerified = verifiedArchs[arch]
	both := arch == "falcon-h1" || !m.LayoutVerified // a layer may have attention and state
	for i := 0; i < nTrunk && i < kvFrom; i++ {
		m.KVLayers[i] = (!m.RecurrentLayers[i] || both) && m.HeadCountKV[i] > 0
	}
	if !anyTrue(m.SWALayers) {
		m.SlidingWindow, m.KeyLengthSWA, m.ValueLengthSWA, m.SWALayers = 0, 0, 0, nil
	}
	if !anyTrue(m.RecurrentLayers) {
		m.RecurrentLayers = nil
	}
	if !m.LayoutVerified {
		m.LayoutNote = "layer rules for " + arch + " not checked against llama.cpp b11541; counts are an upper bound"
	}
	return m, nil
}

func anyTrue(s []bool) bool {
	for _, x := range s {
		if x {
			return true
		}
	}
	return false
}

// at is s[i], false past the end, so a stored ModelInfo with short or empty
// slices is still safe to count.
func at(s []bool, i int) bool { return i >= 0 && i < len(s) && s[i] }

// readTensors reads the tensor-info table and sums tensor bytes by class.
func readTensors(r *bufio.Reader, n uint64, m *ModelInfo) error {
	firstMTP := m.BlockCount - m.NextNLayers
	add := func(dst *int64, b int64) error {
		if *dst > maxBytes-b {
			return errors.New("tensor bytes overflow")
		}
		*dst += b
		return nil
	}
	for i := uint64(0); i < n; i++ {
		name, err := readString(r, maxKeyLen)
		if err != nil {
			return fmt.Errorf("tensor %d: %w", i, err)
		}
		var nd uint32
		if err := binary.Read(r, binary.LittleEndian, &nd); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if nd == 0 || nd > maxDims {
			return fmt.Errorf("%s: %d dimensions", name, nd)
		}
		var dims [maxDims]uint64
		elems := uint64(1)
		for d := uint32(0); d < nd; d++ {
			if err := binary.Read(r, binary.LittleEndian, &dims[d]); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if dims[d] > maxBytes || (dims[d] != 0 && elems > maxBytes/dims[d]) {
				return fmt.Errorf("%s: shape too large", name)
			}
			elems *= dims[d]
		}
		var kind uint32
		var offset uint64
		if err := binary.Read(r, binary.LittleEndian, &kind); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := binary.Read(r, binary.LittleEndian, &offset); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		t, ok := ggmlTypes[kind]
		if !ok {
			return fmt.Errorf("%s: unknown ggml type %d", name, kind)
		}
		if dims[0]%uint64(t.block) != 0 {
			return fmt.Errorf("%s: row of %d is not a whole number of %s blocks", name, dims[0], t.name)
		}
		if elems/uint64(t.block) > maxBytes/uint64(t.size) {
			return fmt.Errorf("%s: too large", name)
		}
		b := int64(elems/uint64(t.block)) * t.size
		if err := add(&m.Bytes.Total, b); err != nil {
			return err
		}
		var class *int64
		switch {
		case strings.HasPrefix(name, "v.") || strings.HasPrefix(name, "mm.") || strings.HasPrefix(name, "a.") || m.Arch == "clip":
			class = &m.Bytes.Projector
		case strings.HasPrefix(name, "nextn.") || strings.Contains(name, ".nextn.") ||
			(m.NextNLayers > 0 && blockIndex(name) >= firstMTP):
			class = &m.Bytes.NextN
		case strings.Contains(name, "_exps"):
			class = &m.Bytes.Experts
		case strings.HasPrefix(name, "token_embd."):
			class = &m.Bytes.TokenEmbedding
		case strings.HasPrefix(name, "output."):
			class = &m.Bytes.Output
		case strings.HasPrefix(name, "per_layer_token_embd."):
			class = &m.Bytes.PerLayerEmbedding
		}
		if class != nil {
			if err := add(class, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// blockIndex is N in a "blk.N." tensor name, -1 for any other name.
func blockIndex(name string) int {
	rest, ok := strings.CutPrefix(name, "blk.")
	if !ok {
		return -1
	}
	num, _, ok := strings.Cut(rest, ".")
	if !ok {
		return -1
	}
	i, err := strconv.Atoi(num)
	if err != nil || i < 0 {
		return -1
	}
	return i
}

// padCells rounds up to llama.cpp's cell padding.
func padCells(n int) int { return (n + cellPad - 1) / cellPad * cellPad }

// swaCells is how many cells one sequence's sliding-window cache holds at
// window ctx: GGML_PAD(min(n_ctx, n_swa + n_ubatch), 256) with the default
// n_ubatch (llama-kv-cache-iswa.cpp, non-unified cache). With --kv-unified
// llama.cpp sizes it n_swa*n_seq + n_ubatch instead; with --swa-full it is
// the whole context.
func (m *ModelInfo) swaCells(ctx int) int {
	base := padCells(ctx)
	return padCells(min(base, m.SlidingWindow+DefaultUBatch))
}

// layerKV is the K plus V elements one token takes in layer i.
func (m *ModelInfo) layerKV(i int) int64 {
	kl, vl := m.KeyLength, m.ValueLength
	if at(m.SWALayers, i) {
		kl, vl = m.KeyLengthSWA, m.ValueLengthSWA
	}
	if i >= len(m.HeadCountKV) {
		return 0
	}
	return int64(m.HeadCountKV[i]) * int64(kl+vl)
}

// kvBytes sums K and V bytes over the layers keep selects.
func (m *ModelInfo) kvBytes(ctx int, elemBytes float64, keep func(i int) bool) int64 {
	if ctx <= 0 || elemBytes <= 0 {
		return 0
	}
	full := padCells(ctx)
	var total float64
	for i, has := range m.KVLayers {
		if !has || !keep(i) {
			continue
		}
		cells := full
		if at(m.SWALayers, i) {
			cells = m.swaCells(ctx)
		}
		total += float64(cells) * float64(m.layerKV(i)) * elemBytes
	}
	return int64(math.Ceil(total))
}

// KVBytes is the KV cache of one sequence at window ctx, in bytes, counting
// only layers that own KV. elemBytes is the cache type's bytes per element
// (f16 2, q8_0 34/32, q4_0 18/32). Like llama.cpp the window is padded to 256
// cells, and a sliding-window layer holds at most window + 512 cells (padded),
// which is what llama.cpp allocates, not what a short prompt fills. It assumes
// flash attention: without it llama.cpp pads V of every layer to the widest.
func (m *ModelInfo) KVBytes(ctx int, elemBytes float64) int64 {
	return m.kvBytes(ctx, elemBytes, func(int) bool { return true })
}

// StateBytes is the recurrent state of one sequence in bytes (f32), summed
// over recurrent layers: n_embd_r + n_embd_s floats per layer as in
// llama-hparams.cpp. llama.cpp keeps 1 + n_rs_seq such rows per sequence;
// n_rs_seq is 0 unless MTP/EAGLE3/DFlash speculation is on, when it is the
// draft length (common.h need_n_rs_seq).
func (m *ModelInfo) StateBytes() int64 {
	var r, s int64
	if m.ShortConvCache > 0 { // LFM2
		r = int64(m.EmbeddingLength) * int64(m.ShortConvCache-1)
	} else if m.SSMConvKernel > 0 {
		r = int64(m.SSMConvKernel-1) * (int64(m.SSMInnerSize) + 2*int64(m.SSMGroupCount)*int64(m.SSMStateSize))
	}
	s = int64(m.SSMStateSize) * int64(m.SSMInnerSize)
	var layers int64
	for i, rec := range m.RecurrentLayers {
		if rec && i < m.BlockCount-m.NextNLayers {
			layers++
		}
	}
	return layers * (r + s) * 4
}

// CheckpointBytes is what one llama-server context checkpoint holds for one
// sequence at window ctx: the part of memory that cannot be rolled back
// (llama_state_seq_get_data with LLAMA_STATE_SEQ_FLAGS_PARTIAL_ONLY, saved by
// server-context.cpp create_checkpoint). That is the recurrent state plus the
// sliding-window KV (at most the SWA cache, an upper bound); full-attention KV
// is never in it. The server makes checkpoints only for recurrent or SWA
// models, so a plain model's is 0. A slot keeps up to --ctx-checkpoints of
// them (default 32). Per-cell and per-layer headers (a few bytes) are left
// out.
func (m *ModelInfo) CheckpointBytes(ctx int, elemBytes float64) int64 {
	return m.StateBytes() + m.kvBytes(ctx, elemBytes, func(i int) bool { return at(m.SWALayers, i) })
}
