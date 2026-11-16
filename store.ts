export interface LabelRecord {
  name: string;
  color: string;
  description: string;
}

export interface CommentRecord {
  id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
}

export interface IssueRecord {
  number: number;
  kind: 'issue';
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  seq: number;
  comments: CommentRecord[];
}

export interface PullRecord {
  number: number;
  kind: 'pull';
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  createdAt: string;
  updatedAt: string;
  seq: number;
  comments: CommentRecord[];
}

export interface RepoRecord {
  id: number;
  owner: string;
  name: string;
  fullName: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  refs: Map<string, string>;
  head: string;
  labels: Map<string, LabelRecord>;
  items: Map<number, IssueRecord | PullRecord>;
  numberSeq: number;
}

export const VIEWER = { login: 'adaiversen', id: 1000, name: 'ada iversen', email: 'adaiversen@example.invalid' } as const;

export class Store {
  repos = new Map<string, RepoRecord>();
  seq = 1;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`.toLowerCase();
  }

  nextId(): number {
    return this.seq++;
  }
}

export const store = new Store();
