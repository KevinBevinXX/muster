import fs from 'node:fs';
import path from 'node:path';

let seq = 0;

// writeFileAtomic replaces `file` with `data` so that a concurrent reader sees
// either the old contents or the new ones, never an empty or half-written file.
//
// 🔴 USE IT FOR EVERY FIXTURE A RUNNING FAKE HOST RE-READS (the windows files,
// the Hermes windows file, a transcript's .status / .picker.json, the active /
// activity files). fs.writeFileSync opens with O_TRUNC and then writes, so a
// host poll (cmd/muster-mirror/e2emirror.go, e2ehermes.go: os.ReadFile every
// MUSTER_MIRROR_E2E_REFRESH) that lands between the two reads an EMPTY file:
// the host then lists no windows for that session, and the hub reports it
// gone ("the host is online but no longer lists this tmux session") — the
// hermes-view flake. The temp file is in the same directory, so the rename is
// one filesystem's atomic replace; its name ends in neither .jsonl nor a
// fixture suffix, so nothing that scans the directory mistakes it for one.
export function writeFileAtomic(file: string, data: string): void {
  const tmp = path.join(path.dirname(file), `.${path.basename(file)}.${process.pid}.${++seq}.tmp`);
  fs.writeFileSync(tmp, data);
  fs.renameSync(tmp, file);
}
