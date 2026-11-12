package gguf

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
)

// u32 writes a uint32 metadata value.
func (w *builder) u32(key string, v uint32) {
	w.kv(key, typeUint32, func() { binary.Write(&w.b, binary.LittleEndian, v) })
}

// ints writes an int32 array metadata value.
func (w *builder) ints(key string, vs ...int32) {
	w.kv(key, typeArray, func() {
		binary.Write(&w.b, binary.LittleEndian, uint32(typeInt32))
		binary.Write(&w.b, binary.LittleEndian, uint64(len(vs)))
		for _, v := range vs {
			binary.Write(&w.b, binary.LittleEndian, v)
		}
	})
}

// bools writes a bool array metadata value.
func (w *builder) bools(key string, vs ...bool) {
	w.kv(key, typeArray, func() {
		binary.Write(&w.b, binary.LittleEndian, uint32(typeBool))
		binary.Write(&w.b, binary.LittleEndian, uint64(len(vs)))
		for _, v := range vs {
			if v {
				w.b.WriteByte(1)
			} else {
				w.b.WriteByte(0)
			}
		}
	})
}

// tensor is one entry of the tensor-info table.
type tensor struct {
	name string
	dims []uint64
	kind uint32
}

// withTensors writes the header with a tensor table after the metadata.
func (w *builder) withTensors(t *testing.T, ts []tensor) string {
	t.Helper()
	var out builder
	out.b.WriteString("GGUF")
	binary.Write(&out.b, binary.LittleEndian, uint32(3))
	binary.Write(&out.b, binary.LittleEndian, uint64(len(ts)))
	binary.Write(&out.b, binary.LittleEndian, w.keys)
	out.b.Write(w.b.Bytes())
	for _, x := range ts {
		out.str(x.name)
		binary.Write(&out.b, binary.LittleEndian, uint32(len(x.dims)))
		for _, d := range x.dims {
			binary.Write(&out.b, binary.LittleEndian, d)
		}
		binary.Write(&out.b, binary.LittleEndian, x.kind)
		binary.Write(&out.b, binary.LittleEndian, uint64(0))
	}
	p := t.TempDir() + "/m.gguf"
	if err := os.WriteFile(p, out.b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustInfo(t *testing.T, p string) *ModelInfo {
	t.Helper()
	m, err := Info(p)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

const q8 = 34.0 / 32.0

// A real model's file, or just its first megabytes: the header is all Info
// reads. BRAINIAC_GGUF_TEST_FILE is the path.
func TestInfoLiveRealFile(t *testing.T) {
	path := os.Getenv("BRAINIAC_GGUF_TEST_FILE")
	if path == "" {
		t.Skip("set BRAINIAC_GGUF_TEST_FILE to read a real file")
	}
	m := mustInfo(t, path)
	b, _ := json.Marshal(m)
	t.Logf("%s", b)
	for _, ctx := range []int{4096, 8192, 32768} {
		t.Logf("ctx %d: kv f16 %d q8_0 %d, state %d, checkpoint f16 %d", ctx,
			m.KVBytes(ctx, 2), m.KVBytes(ctx, q8), m.StateBytes(), m.CheckpointBytes(ctx, 2))
	}
}

// header starts a model header with the keys every architecture has.
func header(arch string, blocks, embd, heads uint32) *builder {
	w := &builder{}
	w.kv("general.architecture", typeString, func() { w.str(arch) })
	w.u32("general.file_type", 15)
	w.u32(arch+".block_count", blocks)
	w.u32(arch+".context_length", 32768)
	w.u32(arch+".embedding_length", embd)
	w.u32(arch+".attention.head_count", heads)
	// a vocabulary-sized array and a long template, which Info must skip
	w.kv("tokenizer.ggml.token_type", typeArray, func() {
		binary.Write(&w.b, binary.LittleEndian, uint32(typeInt32))
		binary.Write(&w.b, binary.LittleEndian, uint64(5000))
		w.b.Write(make([]byte, 4*5000))
	})
	w.kv(templateKey, typeString, func() { w.str(string(make([]byte, 10000))) })
	return w
}

func TestInfoDenseModel(t *testing.T) {
	w := header("llama", 4, 512, 8)
	w.u32("llama.attention.head_count_kv", 2)
	m := mustInfo(t, w.withTensors(t, []tensor{
		{"token_embd.weight", []uint64{512, 1000}, 1},   // F16: 1,024,000
		{"output.weight", []uint64{512, 1000}, 8},       // Q8_0: 16 blocks * 34 * 1000 = 544,000
		{"blk.0.attn_q.weight", []uint64{512, 512}, 12}, // Q4_K: 2 * 144 * 512 = 147,456
		{"output_norm.weight", []uint64{512}, 0},        // F32: 2048
	}))
	if m.KeyLength != 64 || m.ValueLength != 64 { // n_embd / n_head
		t.Fatalf("head size %d/%d", m.KeyLength, m.ValueLength)
	}
	if !m.LayoutVerified || m.FileType != 15 || m.TensorCount != 4 {
		t.Fatalf("%+v", m)
	}
	// 4 layers * 2 kv heads * (64+64) * 2 bytes = 2048 bytes a token; 1000 pads to 1024 cells
	if got := m.KVBytes(1000, 2); got != 2048*1024 {
		t.Fatalf("kv %d", got)
	}
	if got := m.KVBytes(1024, q8); got != 1088*1024 {
		t.Fatalf("kv q8_0 %d", got)
	}
	if m.StateBytes() != 0 || m.CheckpointBytes(4096, 2) != 0 {
		t.Fatalf("a dense model has no state or checkpoint: %d %d", m.StateBytes(), m.CheckpointBytes(4096, 2))
	}
	want := TensorBytes{Total: 1024000 + 544000 + 147456 + 2048, TokenEmbedding: 1024000, Output: 544000}
	if m.Bytes != want {
		t.Fatalf("bytes %+v, want %+v", m.Bytes, want)
	}
}

// qwen35 is the Qwen3.5-2B header: 18 gated-delta-net layers and 6 attention layers.
func qwen35(blocks uint32) *builder {
	w := header("qwen35", blocks, 2048, 8)
	w.u32("qwen35.attention.head_count_kv", 2)
	w.u32("qwen35.attention.key_length", 256)
	w.u32("qwen35.attention.value_length", 256)
	w.u32("qwen35.ssm.conv_kernel", 4)
	w.u32("qwen35.ssm.state_size", 128)
	w.u32("qwen35.ssm.group_count", 16)
	w.u32("qwen35.ssm.time_step_rank", 16)
	w.u32("qwen35.ssm.inner_size", 2048)
	w.u32("qwen35.full_attention_interval", 4)
	return w
}

func TestInfoHybridQwen35(t *testing.T) {
	m := mustInfo(t, qwen35(24).withTensors(t, nil))
	var kv, rec []int
	for i := 0; i < 24; i++ {
		if at(m.KVLayers, i) {
			kv = append(kv, i)
		}
		if at(m.RecurrentLayers, i) {
			rec = append(rec, i)
		}
	}
	if len(kv) != 6 || kv[0] != 3 || kv[5] != 23 || len(rec) != 18 {
		t.Fatalf("attention layers %v, %d recurrent", kv, len(rec))
	}
	// 6 layers * 2 kv heads * (256+256) * 2 bytes = 12,288 bytes a token
	if got := m.KVBytes(4096, 2); got != 12288*4096 {
		t.Fatalf("kv f16 %d", got)
	}
	if got := m.KVBytes(4096, q8); got != 6528*4096 {
		t.Fatalf("kv q8_0 %d", got)
	}
	// per recurrent layer: r = 3*(2048+2*16*128) = 18,432 and s = 128*2048 = 262,144 floats
	if got := m.StateBytes(); got != 20201472 {
		t.Fatalf("state %d", got)
	}
	if got := m.CheckpointBytes(4096, 2); got != 20201472 {
		t.Fatalf("checkpoint %d", got)
	}
}

// An MTP block sits past the trunk: it owns no main KV, no state, and its
// tensors are counted apart.
func TestInfoMTPBlocksAreApart(t *testing.T) {
	w := qwen35(25)
	w.u32("qwen35.nextn_predict_layers", 1)
	m := mustInfo(t, w.withTensors(t, []tensor{
		{"blk.24.nextn.eh_proj.weight", []uint64{4096, 2048}, 1},
		{"blk.24.attn_q.weight", []uint64{2048, 2048}, 1},
		{"blk.23.attn_q.weight", []uint64{2048, 2048}, 1},
	}))
	if m.KVBytes(4096, 2) != 12288*4096 || m.StateBytes() != 20201472 {
		t.Fatalf("kv %d state %d", m.KVBytes(4096, 2), m.StateBytes())
	}
	if m.Bytes.NextN != (4096+2048)*2048*2 || m.Bytes.Total != m.Bytes.NextN+2048*2048*2 {
		t.Fatalf("%+v", m.Bytes)
	}
}

func TestInfoSlidingWindowModels(t *testing.T) {
	// gemma3: a window and the default pattern 6, so layers 5 and 11 are full
	w := header("gemma3", 12, 1024, 4)
	w.u32("gemma3.attention.head_count_kv", 1)
	w.u32("gemma3.attention.key_length", 256)
	w.u32("gemma3.attention.value_length", 256)
	w.u32("gemma3.attention.sliding_window", 1024)
	m := mustInfo(t, w.withTensors(t, nil))
	for i := 0; i < 12; i++ {
		if at(m.SWALayers, i) != (i != 5 && i != 11) {
			t.Fatalf("swa layers %v", m.SWALayers)
		}
	}
	perTok := int64(1 * (256 + 256) * 2)
	// full layers hold 32768 cells, SWA layers 1024 + 512 = 1536
	if got, want := m.KVBytes(32768, 2), perTok*(2*32768+10*1536); got != want {
		t.Fatalf("kv %d, want %d", got, want)
	}
	// a short window caps the SWA layers too
	if got, want := m.KVBytes(1000, 2), perTok*12*1024; got != want {
		t.Fatalf("kv short %d, want %d", got, want)
	}
	if got, want := m.CheckpointBytes(32768, 2), perTok*10*1536; got != want {
		t.Fatalf("checkpoint %d, want %d", got, want)
	}

	// gemma4: the pattern is a bool array, SWA heads differ, the last layers share KV
	w = header("gemma4", 6, 1024, 4)
	w.u32("gemma4.attention.head_count_kv", 2)
	w.u32("gemma4.attention.key_length", 512)
	w.u32("gemma4.attention.value_length", 512)
	w.u32("gemma4.attention.key_length_swa", 256)
	w.u32("gemma4.attention.value_length_swa", 256)
	w.u32("gemma4.attention.sliding_window", 512)
	w.u32("gemma4.attention.shared_kv_layers", 2)
	w.bools("gemma4.attention.sliding_window_pattern", true, true, false, true, true, false)
	m = mustInfo(t, w.withTensors(t, []tensor{{"per_layer_token_embd.weight", []uint64{256, 100}, 0}}))
	// layers 0,1,3 SWA (1024 cells), layer 2 full (8192 cells); 4 and 5 reuse earlier KV
	swa, full := int64(2*(256+256)*2), int64(2*(512+512)*2)
	if got, want := m.KVBytes(8192, 2), 3*swa*1024+full*8192; got != want {
		t.Fatalf("gemma4 kv %d, want %d", got, want)
	}
	if m.Bytes.PerLayerEmbedding != 256*100*4 {
		t.Fatalf("%+v", m.Bytes)
	}

	// gpt-oss: pattern 2, even layers SWA
	w = header("gpt-oss", 4, 512, 8)
	w.u32("gpt-oss.attention.head_count_kv", 1)
	w.u32("gpt-oss.attention.sliding_window", 128)
	m = mustInfo(t, w.withTensors(t, nil))
	if !at(m.SWALayers, 0) || at(m.SWALayers, 1) || !at(m.SWALayers, 2) || at(m.SWALayers, 3) {
		t.Fatalf("gpt-oss swa %v", m.SWALayers)
	}
}

func TestInfoMoECountsExperts(t *testing.T) {
	w := header("qwen3moe", 1, 512, 8)
	w.u32("qwen3moe.expert_count", 8)
	w.u32("qwen3moe.expert_used_count", 2)
	m := mustInfo(t, w.withTensors(t, []tensor{
		{"blk.0.ffn_gate_exps.weight", []uint64{512, 256, 8}, 12}, // Q4_K: 2*144*256*8 = 589,824
		{"blk.0.ffn_down_exps.weight", []uint64{256, 512, 8}, 14}, // Q6_K: 1*210*512*8 = 860,160
		{"blk.0.ffn_gate_shexp.weight", []uint64{512, 256}, 30},   // BF16: 262,144, not an expert
		{"blk.0.ffn_gate_inp.weight", []uint64{512, 8}, 0},        // router F32: 16,384
		{"blk.0.ffn_up_exps.weight", []uint64{512, 256, 8}, 39},   // MXFP4: 16*17*256*8 = 557,056
	}))
	if m.ExpertCount != 8 || m.ExpertUsedCount != 2 {
		t.Fatalf("%d experts, %d used", m.ExpertCount, m.ExpertUsedCount)
	}
	if m.Bytes.Experts != 589824+860160+557056 || m.Bytes.Total != m.Bytes.Experts+262144+16384 {
		t.Fatalf("%+v", m.Bytes)
	}
}

// An architecture whose rules were not checked is counted high, never low.
func TestInfoUncheckedArchIsAnUpperBound(t *testing.T) {
	w := header("nemotron_h", 4, 512, 8)
	w.ints("nemotron_h.attention.head_count_kv", 0, 2, 0, 0)
	w.u32("nemotron_h.ssm.conv_kernel", 4)
	w.u32("nemotron_h.ssm.state_size", 128)
	w.u32("nemotron_h.ssm.inner_size", 1024)
	m := mustInfo(t, w.withTensors(t, nil))
	if m.LayoutVerified || m.LayoutNote == "" {
		t.Fatalf("%+v", m)
	}
	// the one layer with KV heads keeps them, and all four carry a state
	if got := m.KVBytes(256, 2); got != 256*2*(64+64)*2 {
		t.Fatalf("kv %d", got)
	}
	if got := m.StateBytes(); got != 4*(3*1024+128*1024)*4 {
		t.Fatalf("state %d", got)
	}
}

// A stored ModelInfo counts the same as a fresh one.
func TestInfoSurvivesJSON(t *testing.T) {
	m := mustInfo(t, qwen35(24).withTensors(t, nil))
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back ModelInfo
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.KVBytes(8192, 2) != m.KVBytes(8192, 2) || back.StateBytes() != m.StateBytes() {
		t.Fatal("counts changed through JSON")
	}
	if (&ModelInfo{}).KVBytes(4096, 2) != 0 || (&ModelInfo{}).CheckpointBytes(4096, 2) != 0 {
		t.Fatal("an empty info counts memory")
	}
}

// A hostile or broken header fails cleanly instead of counting a wrong number.
func TestInfoRejectsMalformedHeaders(t *testing.T) {
	ok := func() *builder { return header("llama", 2, 512, 8) }
	with := func(f func(w *builder)) *builder { w := ok(); f(w); return w }
	cases := map[string]struct {
		w  *builder
		ts []tensor
	}{
		"unknown ggml type":  {ok(), []tensor{{"a", []uint64{32}, 4}}},
		"future ggml type":   {ok(), []tensor{{"a", []uint64{32}, 43}}},
		"partial block":      {ok(), []tensor{{"a", []uint64{100, 2}, 12}}},
		"five dims":          {ok(), []tensor{{"a", []uint64{1, 1, 1, 1, 1}, 0}}},
		"no dims":            {ok(), []tensor{{"a", nil, 0}}},
		"shape overflow":     {ok(), []tensor{{"a", []uint64{1 << 40, 1 << 40}, 0}}},
		"missing arch":       {&builder{}, nil},
		"too many blocks":    {header("llama", 1000, 512, 8), nil},
		"no blocks":          {header("llama", 0, 512, 8), nil},
		"short kv head list": {with(func(w *builder) { w.ints("llama.attention.head_count_kv", 2) }), nil},
		"negative kv heads":  {with(func(w *builder) { w.ints("llama.attention.head_count_kv", 2, -1) }), nil},
		"gemma4 no pattern":  {header("gemma4", 2, 512, 8), nil},
		"zero interval": {func() *builder {
			w := header("qwen35", 4, 512, 8)
			w.u32("qwen35.full_attention_interval", 0)
			return w
		}(), nil},
	}
	for name, c := range cases {
		if _, err := Info(c.w.withTensors(t, c.ts)); err == nil {
			t.Errorf("%s: read without an error", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}

	// a table that claims more tensors than it has, or an absurd count
	p := ok().withTensors(t, []tensor{{"a", []uint64{32}, 0}})
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]uint64{"truncated tensor table": 2, "huge tensor count": 1 << 62} {
		binary.LittleEndian.PutUint64(data[8:16], n)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Info(p); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

// Every type in the table has a sane bits-per-weight, so a typo in a block
// size shows.
func TestGGMLTypeTable(t *testing.T) {
	for id, ty := range ggmlTypes {
		bpw := float64(ty.size*8) / float64(ty.block)
		if bpw < 1 || bpw > 64 {
			t.Errorf("%d %s: %.3f bits per weight", id, ty.name, bpw)
		}
	}
	if q := ggmlTypes[12]; q.name != "Q4_K" || float64(q.size*8)/float64(q.block) != 4.5 {
		t.Errorf("Q4_K is %+v", q)
	}
}
