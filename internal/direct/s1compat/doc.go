// Package direct (directory s1compat) freezes the direct grant-store reader of
// amux 15fcf0e (#273, schema version 1) for downgrade regression tests.
//
// store.go is that commit's internal/direct/store.go byte for byte (git blob
// 01e13c1f2545b2ffa76c898c38cda56dc5e33bac); it keeps its own package clause
// so it stays verbatim, and TestFrozenReaderIsVerbatim pins its hash. shim.go
// supplies only the declarations store.go referenced from other files of that
// commit: ResourceScope copied verbatim, and an opaque Identity, which the
// grant-file reader never touches. Nothing outside tests imports this package.
package direct
