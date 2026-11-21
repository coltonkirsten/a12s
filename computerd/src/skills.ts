import { existsSync, readFileSync, writeFileSync, renameSync } from 'node:fs';
import { join } from 'node:path';
import type { AgentFsOp } from './agentfs.js';
import type { Config, HarnessKind } from './config.js';
import { agentFs } from './credentials.js';
import type { ControllerReply } from './send.js';

export interface Skill {
  name: string;
  source: 'fleet' | 'agent';
  files: Record<string, string>;
}

interface SkillsResponse {
  storeId: string;
  digest: string;
  skills: Skill[];
}

/** What computerd last wrote: which skill folders are its own, so it removes only those. */
interface Manifest {
  digest: string;
  root: string;
  names: string[];
}

export type ControllerCall = (method: string, path: string, payload?: unknown, headers?: Record<string, string>) => Promise<ControllerReply>;

/** Where each harness reads the user's skills. */
export function skillsRoot(cfg: Config, kind: HarnessKind = cfg.harness.kind): string {
  return kind === 'codex' ? join(cfg.agentHome, '.agents', 'skills') : join(cfg.claudeConfigDir, 'skills');
}

/**
 * The agent computer's skills, kept by the Controller: the Fleet's and the agent's own. computerd copies them into the
 * running harness's skills folder, so a harness switch, a new home drive or a wake brings the same skills.
 */
export class Skills {
  private readonly manifestPath: string;

  constructor(
    private readonly cfg: Config,
    private readonly call: ControllerCall,
    private readonly log: (message: string) => void = () => {},
  ) {
    this.manifestPath = join(cfg.stateDir, 'skills.json');
  }

  private get base(): string {
    return `/v1/agent-computers/${this.cfg.computer}/skills`;
  }

  /** Fetches the skills and writes them out if they changed. Returns their digest, or null when the Controller can't say. */
  async sync(): Promise<string | null> {
    const had = this.manifest();
    const current = had?.root === skillsRoot(this.cfg);
    let reply: ControllerReply;
    try {
      const headers: Record<string, string> = current ? { 'if-none-match': `"${had!.digest}"` } : {};
      reply = await this.call('GET', this.base, undefined, headers);
    } catch (err) {
      this.log(`skills: ${(err as Error).message}`);
      return null;
    }
    if (reply.status === 304 && current) return had!.digest;
    if (reply.status !== 200) {
      // 404 from a Controller without skills: nothing to keep in step with.
      if (reply.status !== 404) this.log(`skills: GET ${reply.status} ${JSON.stringify(reply.body)}`);
      return null;
    }
    const res = reply.body as SkillsResponse;
    this.write(res.skills, res.digest, had);
    return res.digest;
  }

  private write(skills: Skill[], digest: string, had: Manifest | null): void {
    const root = skillsRoot(this.cfg);
    const keep = new Set(skills.map((s) => s.name));
    const ops: AgentFsOp[] = [];
    for (const kind of ['claude-code', 'codex'] as const) {
      const r = skillsRoot(this.cfg, kind);
      for (const name of had?.root === r ? had.names : []) if (r !== root || !keep.has(name)) ops.push({ op: 'remove', path: join(r, name) });
    }
    const ours = new Set(had?.root === root ? had.names : []);
    const listed = agentFs(this.cfg, [{ op: 'list', path: root }])[0];
    const present = new Set(listed?.ok ? (listed.names ?? []) : []);
    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    for (const s of skills) {
      const dir = join(root, s.name);
      if (present.has(s.name) && !ours.has(s.name)) {
        // A folder the agent made by hand under a name the Controller now owns: set aside, never lost.
        ops.push({ op: 'rename', path: dir, target: join(this.cfg.agentHome, '.a12s-replaced-skills', `${s.name}-${stamp}`) });
      } else {
        ops.push({ op: 'remove', path: dir });
      }
      for (const [p, content] of Object.entries(s.files)) {
        ops.push({ op: 'write', path: join(dir, p), content, mode: content.startsWith('#!') ? 0o755 : 0o644 });
      }
    }
    const failed = agentFs(this.cfg, ops).filter((r) => !r.ok);
    if (failed.length) {
      this.log(`skills: ${failed.length} file steps failed: ${failed.map((f) => (f.ok ? '' : f.error)).join('; ')}`);
      return; // no manifest: the next sync writes them again
    }
    const tmp = `${this.manifestPath}.tmp`;
    writeFileSync(tmp, JSON.stringify({ digest, root, names: [...keep].sort() } satisfies Manifest));
    renameSync(tmp, this.manifestPath);
  }

  private manifest(): Manifest | null {
    if (!existsSync(this.manifestPath)) return null;
    try {
      return JSON.parse(readFileSync(this.manifestPath, 'utf8')) as Manifest;
    } catch {
      return null;
    }
  }

  /** save_skill: create or replace one of the agent's own skills, then bring the folder up to date. */
  async save(name: string, files: Record<string, string>): Promise<ControllerReply> {
    const reply = await this.call('PUT', `${this.base}/${encodeURIComponent(name)}`, { files });
    if (reply.status === 200) await this.sync();
    return reply;
  }

  /** delete_skill: remove one of the agent's own skills, then bring the folder up to date. */
  async remove(name: string): Promise<ControllerReply> {
    const reply = await this.call('DELETE', `${this.base}/${encodeURIComponent(name)}`);
    if (reply.status === 200) await this.sync();
    return reply;
  }
}
