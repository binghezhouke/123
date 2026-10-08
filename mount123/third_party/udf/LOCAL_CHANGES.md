# Local changes

This is a minimal local fork of [`golift.io/udf`](https://github.com/golift/udf)
at upstream commit
[`4fae2a5ff797070a6fa5434cad55f0964ba117e6`](https://github.com/golift/udf/tree/4fae2a5ff797070a6fa5434cad55f0964ba117e6).
The upstream BSD 3-Clause license is preserved in [`LICENSE`](LICENSE).

The local change replaces recursive allocation-descriptor-chain expansion
with bounded iterative depth-first expansion. It preserves extent ordering and
hole handling, rejects repeated next-descriptor locations, and caps cumulative
chain reads and output runs. The public API remains unchanged.
