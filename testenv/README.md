# Local test environment

A real Dovecot IMAP server and a separate `NOUGHT_HOME`, for developing and
testing nought **without ever touching your real mail**.

```sh
make testenv-up      # start Dovecot on 127.0.0.1:10993 (TLS) and :10143 (STARTTLS)
make testenv-seed    # fill alice@nought.test and bob@nought.test with varied mail
make integration     # run the integration tests (fresh random user per test)
make testenv-down    # stop (keeps the mail); make testenv-reset wipes it and testenv/home
```

Trying `nou` by hand:

```sh
make dev ARGS=version                         # builds testenv/nou
testenv/dev.sh account add alice --email alice@nought.test --imap-host 127.0.0.1 \
    --imap-port 10993 --password-command 'echo nought-test-password'
testenv/dev.sh sync --dry-run
testenv/dev.sh search tag:inbox
```

- Every user name works; the password is always `nought-test-password`.
- Folders use `.` as the hierarchy separator, like many real servers.
- `testenv/dev.sh` sets `NOU_DEV=1`, `NOUGHT_HOME=testenv/home` and `NOU_DEV_CA`
  (the throwaway certificate from `gen-cert.sh`). With `NOU_DEV=1`, `nou` refuses
  to run without `NOUGHT_HOME`, so a dev run can never fall back to your real data.
- The container belongs to the Compose project `nought-testenv`; nothing else is
  touched. `testenv/home`, `testenv/certs` and `testenv/nou` are gitignored.
