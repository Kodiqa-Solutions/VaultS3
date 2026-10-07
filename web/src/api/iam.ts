import { apiFetch } from './client'
import { encodeSegment } from './paths'

export interface IAMUser {
  name: string
  policyArns: string[]
  groups: string[]
  allowedCidrs: string[]
  createdAt: string
}

export interface IAMGroup {
  name: string
  policyArns: string[]
  createdAt: string
}

export interface IAMPolicy {
  name: string
  document: string
  createdAt: string
}

export function listUsers(): Promise<IAMUser[]> {
  return apiFetch<IAMUser[]>('/iam/users')
}

export function createUser(name: string): Promise<IAMUser> {
  return apiFetch<IAMUser>('/iam/users', { method: 'POST', body: JSON.stringify({ name }) })
}

export async function deleteUser(name: string): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(name)}`, { method: 'DELETE' })
}

export async function attachUserPolicy(userName: string, policyName: string): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(userName)}/policies`, { method: 'POST', body: JSON.stringify({ policyName }) })
}

export async function detachUserPolicy(userName: string, policyName: string): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(userName)}/policies/${encodeSegment(policyName)}`, { method: 'DELETE' })
}

export async function addUserToGroup(userName: string, groupName: string): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(userName)}/groups`, { method: 'POST', body: JSON.stringify({ groupName }) })
}

export async function removeUserFromGroup(userName: string, groupName: string): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(userName)}/groups/${encodeSegment(groupName)}`, { method: 'DELETE' })
}

export function listGroups(): Promise<IAMGroup[]> {
  return apiFetch<IAMGroup[]>('/iam/groups')
}

export function createGroup(name: string): Promise<IAMGroup> {
  return apiFetch<IAMGroup>('/iam/groups', { method: 'POST', body: JSON.stringify({ name }) })
}

export async function deleteGroup(name: string): Promise<void> {
  return apiFetch<void>(`/iam/groups/${encodeSegment(name)}`, { method: 'DELETE' })
}

export async function attachGroupPolicy(groupName: string, policyName: string): Promise<void> {
  return apiFetch<void>(`/iam/groups/${encodeSegment(groupName)}/policies`, { method: 'POST', body: JSON.stringify({ policyName }) })
}

export async function detachGroupPolicy(groupName: string, policyName: string): Promise<void> {
  return apiFetch<void>(`/iam/groups/${encodeSegment(groupName)}/policies/${encodeSegment(policyName)}`, { method: 'DELETE' })
}

export function listPolicies(): Promise<IAMPolicy[]> {
  return apiFetch<IAMPolicy[]>('/iam/policies')
}

export function createPolicy(name: string, document: string): Promise<IAMPolicy> {
  return apiFetch<IAMPolicy>('/iam/policies', { method: 'POST', body: JSON.stringify({ name, document }) })
}

export async function deletePolicy(name: string): Promise<void> {
  return apiFetch<void>(`/iam/policies/${encodeSegment(name)}`, { method: 'DELETE' })
}

export async function setIPRestrictions(userName: string, allowedCidrs: string[]): Promise<void> {
  return apiFetch<void>(`/iam/users/${encodeSegment(userName)}/ip-restrictions`, { method: 'PUT', body: JSON.stringify({ allowedCidrs }) })
}
