# Encrypted ZIP interoperability fixtures

These small test archives were generated with tools independent of the Go reader:

- `aes128-store.zip`, `aes192-deflate.zip`, `aes256-deflate.zip`: Python `pyzipper`, WinZip AES at the indicated strength and compression method.
- `aes256-ae2-store.zip`, `aes256-ae2-deflate.zip`: explicit AE-2 variants, using `force_wz_aes_version=2`; these omit the plaintext CRC and require authentication to detect corruption.
- `zipcrypto-store.zip`, `zipcrypto-deflate.zip`: Info-ZIP `zip -0` / `zip -9`, with `-j -P mount-test-password`.

Each archive contains `hello.txt`. The public test password is `mount-test-password`.
The exact plaintext is the Python expression:

```python
(b"mount123 encrypted fixture\n" * 256) + bytes(range(256))
```

AES fixture generation uses `pyzipper.AESZipFile(..., encryption=pyzipper.WZ_AES)`,
`setpassword(b"mount-test-password")`, `setencryption(pyzipper.WZ_AES, nbits=128|192|256)`,
and `writestr("hello.txt", payload)`. Salts and encryption headers are randomized by the generating tools.

7z fixtures generated with py7zr: `plain.7z` (unencrypted), `password.7z`
(encrypted contents), `copy.7z` (AES-encrypted Copy method, explicit CRC validation), `headers.7z` (encrypted headers and contents), `solid.7z`
(encrypted headers and two files in a solid stream). Test-only password:
`mount-test-password`. The first payload is `mount 7z fixture contents\n`
repeated 200 times; the solid archive also contains `last solid member\n`.
RAR fixtures and their license are reused from `tests/fixtures/rar` in the
repository root. Their expected SHA-256 was independently verified with unrar.
