# Restore

1. Install the server binary on the replacement host and give it the same static IP (192.168.1.150).
2. Run `deploy/systemd/setup-server.sh`.
3. Fetch the newest archive from the backup host and decrypt with the age private key from the password manager:
   `age -d -i backup.key -o kumaboard.tar kumaboard-<stamp>.tar.age`
4. `tar -C / -xf kumaboard.tar` restores `/var/lib/kumaboard/pki`, `/var/lib/kumaboard/releases`, and `/etc/kumaboard`.
   Move the extracted `kumaboard.db` (at the tar root) to `/var/lib/kumaboard/kumaboard.db`.
5. `chown -R kumaboard:kumaboard /var/lib/kumaboard` and `chmod 0600 /var/lib/kumaboard/pki/*.key`.
6. `systemctl start kumaboard`.

Agents reconnect on their own: same IP, same CA, same token hashes. Nothing is reinstalled on any device.

## Drill (do this at phase 1 acceptance)

Restore the newest archive into a scratch directory on the backup host, start a second server from it on another port, and log in:

    mkdir -p /tmp/restore && cd /tmp/restore
    age -d -i backup.key -o kb.tar kumaboard-<stamp>.tar.age && tar -xf kb.tar
    mkdir -p data && mv kumaboard.db data/ && mv var/lib/kumaboard/pki data/
    printf 'listen_addrs: ["127.0.0.1:8444"]\ndata_dir: /tmp/restore/data\n' > config.yaml
    kumaboard serve -config config.yaml

`curl -k https://127.0.0.1:8444/api/login` with the operator password and then `/api/devices` must list every device.

## Moving the server to a new host

This is the same procedure, plus: edit each agent's `server.url` to the new host's IP, and run `kumaboard cert reissue` on the new host so the certificate carries the new SAN. The CA does not change, so agents need no new `ca.pem`.
