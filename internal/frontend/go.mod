// The C front end: lexer, parser, type system, semantic checks and the AST.
//
// It is a separate module with no dependencies -- not even on the assembler --
// so that a second compiler can be built on the same front end without
// inheriting anything from the first. src/goc (the native code generator) and
// src/gocl (the LLVM one) both require this module; nothing here knows which
// backend will consume its output.
module goc/frontend

go 1.21
