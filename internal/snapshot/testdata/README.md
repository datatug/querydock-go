The three published fixtures are byte-for-byte copies from datatug-apps commit
2220f62c091488ffa229cbd787550d2c063984fa. `provenance.json` records their hashes
and the three owning TypeScript source hashes. Tests pin both origin and bytes.

`number-vectors.json` is additional cross-runtime evidence, not an upstream
fixture. It stores IEEE-754 binary64 bits and Node 24.20.0 `JSON.stringify`
output: 22 boundary values from RFC 8785 Appendix B followed by 256 finite
samples from an unsigned 64-bit LCG (seed 0x123456789abcdef, multiplier
6364136223846793005, increment 1442695040888963407, modulo 2^64). Non-finite
encodings are skipped. The source validator rejects non-finite numbers.
The same deterministic local experiment tested 32,782 finite values; the
bounded checked-in sample keeps routine tests small. Go tests can run a larger
fixture with QUERYDOCK_NUMBER_PARITY_FIXTURE=/absolute/path/to/vectors.json.

RFC 8785: https://www.rfc-editor.org/rfc/rfc8785.html#appendix-B
