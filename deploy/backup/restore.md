# Backup and restore

## Install the nightly backup

On the control plane host, as root, after `deploy/systemd/setup-server.sh`:

    deploy/backup/install.sh

It installs `backup.sh`, the service, the failure hook and the timer, and creates
`/etc/kumaboard/backup.env` from `backup.env.example`. It then lists what is still missing
(the backup host, the age public key, the SSH key); the timer is enabled once all three are in
place, so re-run it after supplying them. Run one backup by hand and check it arrived:

    systemctl start kumaboard-backup.service
    journalctl -u kumaboard-backup.service -n 20

A failed run starts `kumaboard-backup-failed.service`, which logs at `crit` and runs
`/etc/kumaboard/backup-failed.sh` if that file exists. Put a notification there.

Each archive is `kumaboard-<stamp>.tar.age` and holds, relative to its root: `kumaboard.db`,
`config.yaml`, `pki/` and `releases/`.

## Restore

1. Install the server binary on the replacement host and give it the address the agents
   already dial.
2. Run `deploy/systemd/setup-server.sh`.
3. Fetch the newest archive from the backup host and decrypt it with the age private key from
   the password manager:
   `age -d -i backup.key -o kumaboard.tar kumaboard-<stamp>.tar.age`
4. Unpack it into the data directory and put the config in place:

       tar -C /var/lib/kumaboard -xf kumaboard.tar
       mv /var/lib/kumaboard/config.yaml /etc/kumaboard/config.yaml

   Check `data_dir` in the config says `/var/lib/kumaboard`.
5. Give the data to the service user, then take the release signing key back. The server must
   not be able to read it; only `kumaboard sign`, run as root, uses it.

       chown -R kumaboard:kumaboard /var/lib/kumaboard
       chmod 0600 /var/lib/kumaboard/pki/*.key
       chown root:root /var/lib/kumaboard/pki/release_ed25519
       chmod 0600 /var/lib/kumaboard/pki/release_ed25519

6. `systemctl start kumaboard`.

Agents reconnect on their own: same address, same CA, same token hashes. Nothing is
reinstalled on any device.

## Drill (do this at phase 1 acceptance)

Restore the newest archive into a scratch directory on the backup host, start a second server
from it on another port, and log in:

    mkdir -p /tmp/restore/data && cd /tmp/restore
    age -d -i backup.key -o kb.tar kumaboard-<stamp>.tar.age && tar -C data -xf kb.tar
    printf 'listen_addrs: ["127.0.0.1:8444"]\ndata_dir: /tmp/restore/data\n' > config.yaml
    kumaboard serve -config config.yaml

`curl -k https://127.0.0.1:8444/api/login` with the operator password and then `/api/devices`
must list every device.

## Moving a local run to the production layout

A server started from a data directory inside a checkout (a `data-local/` run) moves the same
way as a restore, without the archive. As root, with the old server stopped:

    deploy/systemd/setup-server.sh
    install -m 0755 bin/kumaboard /opt/kumaboard/kumaboard
    cp -a <old data dir>/kumaboard.db <old data dir>/pki <old data dir>/releases /var/lib/kumaboard/
    install -m 0644 <old config.yaml> /etc/kumaboard/config.yaml   # then set data_dir: /var/lib/kumaboard

Copy the database only while the old server is stopped, so no `-wal` file is left behind. Then
apply the ownership in restore step 5, start the service, and install the backup. Keep the old
directory until agents have reconnected and one backup has been restored in the drill.

## Moving the server to a new host

This is the restore procedure, plus: edit each agent's `server.url` to the new host's address,
and run `kumaboard cert reissue` on the new host so the certificate carries the new SAN. The CA
does not change, so agents need no new `ca.pem`.
