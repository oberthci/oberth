package staticgit

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func record(body []byte) Record {
	return Record{Architecture: AMD64, DistributionVersion: "3.23.8", GitVersion: "2.56.0", GitSHA256: hash(body),
		SourceSHA256: strings.Repeat("a", 64), PatchSHA256: strings.Repeat("b", 64),
		MuslVersion: "1.2.5", MuslSHA256: hash([]byte("musl")),
		ZlibVersion: "1.4", ZlibSHA256: hash([]byte("zlib"))}
}

func executable(machine elf.Machine) []byte {
	body := make([]byte, 64)
	copy(body, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(body[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(body[18:], uint16(machine))
	binary.LittleEndian.PutUint32(body[20:], 1)
	binary.LittleEndian.PutUint16(body[52:], 64)
	binary.LittleEndian.PutUint16(body[58:], 64)
	return body
}

func TestStaticBuildBindsEveryActualComponent(t *testing.T) {
	for _, arch := range []Architecture{AMD64, ARM64} {
		machine := elf.EM_X86_64
		if arch == ARM64 {
			machine = elf.EM_AARCH64
		}
		body := executable(machine)
		r := record(body)
		r.Architecture = arch
		if err := r.VerifyBuild(body, []byte("musl"), []byte("zlib"), installedPackages()); err != nil {
			t.Fatal(err)
		}
		for _, changed := range []struct{ git, musl, zlib []byte }{
			{[]byte("changed"), []byte("musl"), []byte("zlib")},
			{body, []byte("changed"), []byte("zlib")},
			{body, []byte("musl"), []byte("changed")},
		} {
			if r.VerifyBuild(changed.git, changed.musl, changed.zlib, installedPackages()) == nil {
				t.Fatal("changed linked component accepted")
			}
		}
	}
}

func TestStaticRecordRefusesUnboundOrUnsupportedInputs(t *testing.T) {
	for name, mutate := range map[string]func(*Record){
		"architecture":    func(r *Record) { r.Architecture = "unknown" },
		"version":         func(r *Record) { r.GitVersion = "" },
		"library_version": func(r *Record) { r.MuslVersion = "bad version" },
		"binary":          func(r *Record) { r.GitSHA256 = "" },
		"source":          func(r *Record) { r.SourceSHA256 = "" },
		"patch":           func(r *Record) { r.PatchSHA256 = "" },
		"library":         func(r *Record) { r.ZlibSHA256 = strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			r := record(executable(elf.EM_X86_64))
			mutate(&r)
			if r.Validate() == nil {
				t.Fatal("invalid record accepted")
			}
			if _, err := r.SBOM(); err == nil {
				t.Fatal("invalid record produced an inventory")
			}
		})
	}
	if Architecture("").Valid() {
		t.Fatal("zero architecture is valid")
	}
}

func TestStaticBinaryRefusesWrongArchitectureFormatAndInterpreter(t *testing.T) {
	for name, body := range map[string][]byte{
		"malformed":       []byte("not an executable"),
		"wrong_machine":   executable(elf.EM_AARCH64),
		"interpreter":     interpreted(),
		"dynamic_library": dynamicLibrary(),
	} {
		t.Run(name, func(t *testing.T) {
			r := record(body)
			if r.ValidateBinary(body) == nil {
				t.Fatal("non-static or wrong-target executable accepted")
			}
		})
	}
}

func TestSBOMIsStableAndLabelsAreIndependent(t *testing.T) {
	r := record(executable(elf.EM_X86_64))
	first, err := r.SBOM()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.SBOM()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("component inventory is unstable")
	}
	var bom struct {
		Components []component `json:"components"`
	}
	if err := json.Unmarshal(first, &bom); err != nil {
		t.Fatal(err)
	}
	if len(bom.Components) != 3 || bom.Components[0].Hashes[0].Content != r.GitSHA256 || bom.Components[0].CPE == "" {
		t.Fatal("inventory omits actual linked components or executable identity")
	}
	labels := r.Labels()
	labels["org.oberth.static-git.version"] = "changed"
	if r.Labels()["org.oberth.static-git.version"] != r.GitVersion {
		t.Fatal("label maps share mutable state")
	}
}

func interpreted() []byte {
	body := append(executable(elf.EM_X86_64), make([]byte, 56)...)
	binary.LittleEndian.PutUint64(body[32:], 64)
	binary.LittleEndian.PutUint16(body[54:], 56)
	binary.LittleEndian.PutUint16(body[56:], 1)
	binary.LittleEndian.PutUint32(body[64:], uint32(elf.PT_INTERP))
	return body
}

func dynamicLibrary() []byte {
	body := append(executable(elf.EM_X86_64), make([]byte, 32)...)
	binary.LittleEndian.PutUint64(body[64:], uint64(elf.DT_NEEDED))
	binary.LittleEndian.PutUint64(body[72:], 1)
	stringsOffset := len(body)
	body = append(body, []byte("\x00libc.so.6\x00")...)
	namesOffset := len(body)
	names := []byte("\x00.dynamic\x00.dynstr\x00.shstrtab\x00")
	body = append(body, names...)
	sectionOffset := len(body)
	body = append(body, make([]byte, 4*64)...)
	binary.LittleEndian.PutUint64(body[40:], uint64(sectionOffset))
	binary.LittleEndian.PutUint16(body[60:], 4)
	binary.LittleEndian.PutUint16(body[62:], 3)
	section(body[sectionOffset+64:], [4]uint64{1, uint64(elf.SHT_DYNAMIC), 64, 32})
	binary.LittleEndian.PutUint32(body[sectionOffset+64+40:], 2)
	binary.LittleEndian.PutUint64(body[sectionOffset+64+56:], 16)
	section(body[sectionOffset+128:], [4]uint64{10, uint64(elf.SHT_STRTAB), uint64(stringsOffset), 11})
	section(body[sectionOffset+192:], [4]uint64{18, uint64(elf.SHT_STRTAB), uint64(namesOffset), uint64(len(names))})
	return body
}

func section(body []byte, fields [4]uint64) {
	binary.LittleEndian.PutUint32(body, uint32(fields[0]))
	binary.LittleEndian.PutUint32(body[4:], uint32(fields[1]))
	binary.LittleEndian.PutUint64(body[24:], fields[2])
	binary.LittleEndian.PutUint64(body[32:], fields[3])
}
