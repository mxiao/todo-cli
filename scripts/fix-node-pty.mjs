// node-pty ships its macOS prebuilt `spawn-helper` without the executable
// bit in some releases, which makes every spawn fail with "posix_spawnp
// failed". Restore the bit after install; a no-op when nothing needs fixing.
import { chmodSync, existsSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const prebuilds = join('node_modules', 'node-pty', 'prebuilds');
if (existsSync(prebuilds)) {
  for (const platform of readdirSync(prebuilds)) {
    const helper = join(prebuilds, platform, 'spawn-helper');
    if (existsSync(helper)) chmodSync(helper, 0o755);
  }
}
