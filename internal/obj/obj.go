// Package obj is the in-memory object format shared by goc32's assembler and
// linker.  It is deliberately small: sections with a byte buffer (or a size,
// for .bss), the relocations the assembler could not resolve inside one
// section, and the symbol table the linker needs to resolve the rest.
//
// There is no on-disk form.  goc32 assembles each translation unit into one
// of these and links them in the same process, which is why the assembler and
// linker can agree on a relocation kind instead of re-deriving one from an
// ELF relocation type and a symbol index.
package obj

// Section flags, matching the ELF values so the writer can copy them.
const (
	SHF_WRITE     = 1
	SHF_ALLOC     = 2
	SHF_EXECINSTR = 4
)

// Section types, matching ELF.
const (
	SHT_PROGBITS = 1
	SHT_NOTE     = 7
	SHT_NOBITS   = 8
)

// RelKind says how to patch the bytes at Reloc.Off once the symbol address is
// known.  The two PC-relative kinds patch a second instruction at a fixed
// offset from Off (auipc+addi for an address, auipc+jalr for a call), because
// the RISC-V hi/lo split has to be computed from the same place value.
type RelKind uint8

const (
	Abs32    RelKind = iota // 32-bit absolute address (dynamic RELATIVE at load)
	RVPCRel                 // auipc rd, %pcrel_hi(S+A) ; addi rd, rd, %pcrel_lo
	RVCall                  // auipc ra, %pcrel_hi(S+A) ; jalr ra, ra, %pcrel_lo
	RVJal                   // jal rd, S+A (J-type immediate)
	RVBranch                // b<c>, S+A (B-type immediate)
	ARMBl                   // bl S+A (Thumb BL, PC relative)
)

// Reloc is one unresolved reference.
type Reloc struct {
	Off    int
	Kind   RelKind
	Sym    string
	Addend int64
}

// Sym is a label.  Sec is nil until the label is defined; a symbol that is
// still nil after every object has been assembled is an unresolved reference.
// Value is the offset from the start of Sec.
type Sym struct {
	Name   string
	Sec    *Sec
	Value  int
	Global bool
	Func   bool // function symbol; ARM needs bit 0 set when its address is used
	Addr   uint32
}

// Sec is one input section.  Data is empty for SHT_NOBITS; Size is always the
// in-memory size.
type Sec struct {
	Name   string
	Flags  uint32
	Type   uint32
	Align  int
	Size   int
	Data   []byte
	Relocs []Reloc

	// Filled in by the linker.
	OutAddr uint32
	Live    bool
}

// Add appends bytes to a PROGBITS section, growing Size with it.
func (s *Sec) Add(b []byte) {
	s.Data = append(s.Data, b...)
	s.Size = len(s.Data)
}

// AddZero appends n zero bytes.  It writes them for PROGBITS sections and
// only accounts for them in NOBITS sections, whose contents never reach the
// file.
func (s *Sec) AddZero(n int) {
	if n <= 0 {
		return
	}
	if s.Type == SHT_NOBITS {
		s.Size += n
		return
	}
	s.Data = append(s.Data, make([]byte, n)...)
	s.Size = len(s.Data)
}

// Object is one assembled translation unit.
type Object struct {
	Path string
	Secs []*Sec
	Syms map[string]*Sym
}

func New(path string) *Object {
	return &Object{Path: path, Syms: map[string]*Sym{}}
}

// Sec returns the named section, creating it with the given attributes when
// it does not exist yet.  Re-opening a section keeps its original attributes:
// that is what a `.section` directive that repeats the same name depends on.
func (o *Object) Sec(name string, flags, typ uint32, align int) *Sec {
	for _, s := range o.Secs {
		if s.Name == name {
			return s
		}
	}
	if align < 1 {
		align = 1
	}
	s := &Sec{Name: name, Flags: flags, Type: typ, Align: align}
	o.Secs = append(o.Secs, s)
	return s
}

// Sym returns the named symbol, creating an undefined one on first use.
func (o *Object) Sym(name string) *Sym {
	if s, ok := o.Syms[name]; ok {
		return s
	}
	s := &Sym{Name: name}
	o.Syms[name] = s
	return s
}

// Define binds a symbol to a section offset.  Defining the same name twice is
// an error the assembler reports with the file and line.
func (o *Object) Define(name string, sec *Sec, off int, global, fn bool) {
	s := o.Sym(name)
	s.Sec, s.Value, s.Global, s.Func = sec, off, global, fn
}

// Section returns the section that defines the named symbol in this object,
// or nil when the symbol is external or undefined.
func (o *Object) Section(name string) *Sec {
	if s, ok := o.Syms[name]; ok && s.Sec != nil {
		return s.Sec
	}
	return nil
}
