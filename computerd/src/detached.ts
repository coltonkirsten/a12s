import { randomBytes } from 'node:crypto';
import { closeSync, existsSync, fstatSync, openSync, readdirSync, readFileSync, readlinkSync, readSync } from 'node:fs';

/**
 * Every harness process carries this variable, and so does everything it starts: a process that left the harness's
 * tree (`nohup … &`, `setsid`, a double fork) is still recognisable as the agent's work.
 */
export const MARK_VAR = 'A12S_HARNESS';
export const MARK = randomBytes(8).toString('hex');

export interface Proc {
  pid: number;
  ppid: number;
  /** Start time in clock ticks since boot: with the pid, it tells a process from a later one that reused the pid. */
  start: string;
  marked: boolean;
  command: string;
  /** The regular file its stdout goes to, when it goes to one. */
  output: string | null;
}

export interface ProcessTable {
  list(): Proc[];
  kill(pid: number, signal: NodeJS.Signals): void;
}

/** The process table as /proc shows it. */
export class LinuxProcesses implements ProcessTable {
  constructor(
    private readonly root = '/proc',
    private readonly mark = `${MARK_VAR}=${MARK}`,
  ) {}

  static available(): boolean {
    return existsSync('/proc/self/stat');
  }

  list(): Proc[] {
    const out: Proc[] = [];
    for (const name of readdirSync(this.root)) {
      if (!/^\d+$/.test(name)) continue;
      const dir = `${this.root}/${name}`;
      try {
        const stat = readFileSync(`${dir}/stat`, 'utf8');
        // The command name in parentheses may itself contain spaces and parentheses.
        const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
        if (fields[0] === 'Z' || fields[0] === 'X') continue;
        out.push({
          pid: Number(name),
          ppid: Number(fields[1]),
          start: fields[19] ?? '',
          marked: this.marked(dir),
          command: readFileSync(`${dir}/cmdline`, 'utf8').split('\0').filter(Boolean).join(' '),
          output: outputFile(dir),
        });
      } catch {
        // gone between readdir and read
      }
    }
    return out;
  }

  kill(pid: number, signal: NodeJS.Signals): void {
    try {
      process.kill(pid, signal);
    } catch {
      // already gone
    }
  }

  private marked(dir: string): boolean {
    try {
      return readFileSync(`${dir}/environ`, 'utf8').split('\0').includes(this.mark);
    } catch {
      return false;
    }
  }
}

function outputFile(dir: string): string | null {
  try {
    const target = readlinkSync(`${dir}/fd/1`);
    return target.startsWith('/') && !target.startsWith('/dev/') && !target.endsWith(' (deleted)') ? target : null;
  } catch {
    return null;
  }
}

/** The last `max` characters of a file, or null when it cannot be read. */
export function tailOf(path: string, max = 2000): string | null {
  let fd: number | undefined;
  try {
    fd = openSync(path, 'r');
    const size = fstatSync(fd).size;
    const len = Math.min(size, max * 4);
    const buf = Buffer.alloc(len);
    readSync(fd, buf, 0, len, size - len);
    return buf.toString('utf8').slice(-max);
  } catch {
    return null;
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

/** One piece of detached work: the process that left the harness's tree and everything it started since. */
export interface DetachedTask {
  id: string;
  pid: number;
  command: string;
  output: string | null;
  /** The turn running when it was found, if any: work that ends inside that same turn is not background work. */
  turn: string | null;
  members: Map<string, number>;
}

const key = (p: Proc) => `${p.pid}:${p.start}`;

/**
 * Detached processes the harness's session started: a marked process whose parent is neither marked nor computerd
 * itself (the harness's own parent). A task lives while any process in its tree does, so a daemon that forks again
 * and exits stays one task.
 */
export class Detached {
  private readonly tasks = new Map<string, DetachedTask>();

  constructor(
    readonly table: ProcessTable,
    private readonly self = process.pid,
  ) {}

  get size(): number {
    return this.tasks.size;
  }

  has(id: string): boolean {
    return this.tasks.has(id);
  }

  /** Rescans the table: tasks that appeared, and tasks whose every process is gone. */
  scan(turn: string | null): { started: DetachedTask[]; ended: DetachedTask[] } {
    const procs = this.table.list();
    const byKey = new Map(procs.map((p) => [key(p), p]));
    const byPid = new Map(procs.map((p) => [p.pid, p]));
    const children = new Map<number, Proc[]>();
    for (const p of procs) children.set(p.ppid, [...(children.get(p.ppid) ?? []), p]);
    const tree = (seeds: Proc[]): Map<string, number> => {
      const members = new Map<string, number>();
      const queue = [...seeds];
      for (let p = queue.pop(); p; p = queue.pop()) {
        if (members.has(key(p))) continue;
        members.set(key(p), p.pid);
        queue.push(...(children.get(p.pid) ?? []));
      }
      return members;
    };

    const ended: DetachedTask[] = [];
    const claimed = new Set<string>();
    for (const [id, t] of this.tasks) {
      t.members = tree([...t.members.keys()].map((k) => byKey.get(k)).filter((p): p is Proc => p !== undefined));
      if (t.members.size === 0) {
        this.tasks.delete(id);
        ended.push(t);
      } else for (const k of t.members.keys()) claimed.add(k);
    }

    const started: DetachedTask[] = [];
    for (const p of procs) {
      if (!p.marked || p.ppid === this.self || byPid.get(p.ppid)?.marked || claimed.has(key(p))) continue;
      const t: DetachedTask = { id: `process-${p.pid}`, pid: p.pid, command: p.command.slice(0, 300), output: p.output, turn, members: tree([p]) };
      for (const k of t.members.keys()) claimed.add(k);
      this.tasks.set(t.id, t);
      started.push(t);
    }
    return { started, ended };
  }

  /** SIGTERM to the task's processes, SIGKILL to what is left after `killAfterMs`. False when there is no such task. */
  stop(id: string, killAfterMs: number): boolean {
    const t = this.tasks.get(id);
    if (!t) return false;
    this.signal(t, killAfterMs);
    return true;
  }

  /** Ends every task with the session that started it; they are forgotten at once, never reported. */
  killAll(killAfterMs: number): DetachedTask[] {
    const all = [...this.tasks.values()];
    this.tasks.clear();
    for (const t of all) this.signal(t, killAfterMs);
    return all;
  }

  /** Kills processes that left the harness's tree only as it exited: they were never reported as started. */
  sweep(killAfterMs: number): void {
    this.scan(null);
    this.killAll(killAfterMs);
  }

  private signal(t: DetachedTask, killAfterMs: number): void {
    const members = new Map(t.members);
    for (const pid of members.values()) this.table.kill(pid, 'SIGTERM');
    const timer = setTimeout(() => {
      // Only processes that are still the same ones: a pid may have been reused meanwhile.
      const alive = new Set(this.table.list().map(key));
      for (const [k, pid] of members) if (alive.has(k)) this.table.kill(pid, 'SIGKILL');
    }, killAfterMs);
    timer.unref();
  }
}

const attr = (s: string) => s.replace(/[\r\n]+/g, ' ').replace(/[&"<>]/g, (c) => ({ '&': '&amp;', '"': '&quot;', '<': '&lt;', '>': '&gt;' })[c]!);

/** The prompt of the turn that reports detached work that ended. */
export function detachedPrompt(ended: { task: DetachedTask; tail: string | null }[]): string {
  const body = ended.map(({ task, tail }) => {
    const output = task.output ? ` output="${attr(task.output)}"` : '';
    const text = tail === null ? '(its output went to no file a12s can read; redirect it to a file to see it here)' : tail.trim();
    return `<background-result kind="process" command="${attr(task.command)}" pid="${task.pid}"${output}>\n${text}\n</background-result>`;
  });
  return `Background processes you started have exited (a12s cannot see their exit codes).\n${body.join('\n')}`;
}
