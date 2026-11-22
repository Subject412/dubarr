import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query'
import {
  api,
  type AuthStatus,
  type BrowseResponse,
  type Health,
  type QueueResponse,
  type QueueTransition,
  type SettingsEnvelope,
  type User,
} from './api'

export const queryKeys = {
  authStatus: ['auth', 'status'] as const,
  me: ['auth', 'me'] as const,
  settings: ['settings'] as const,
  health: ['health'] as const,
  queue: ['queue'] as const,
  browse: (path: string) => ['browse', path] as const,
}

/** Public first-run probe; drives the route gate. */
export function useAuthStatus(): UseQueryResult<AuthStatus> {
  return useQuery({
    queryKey: queryKeys.authStatus,
    queryFn: api.authStatus,
    retry: false,
    staleTime: 5_000,
  })
}

export function useMe(enabled = true): UseQueryResult<User> {
  return useQuery({
    queryKey: queryKeys.me,
    queryFn: api.me,
    enabled,
    retry: false,
  })
}

export function useSettings(enabled = true): UseQueryResult<SettingsEnvelope> {
  return useQuery({
    queryKey: queryKeys.settings,
    queryFn: api.settings,
    enabled,
  })
}

export function useHealth(): UseQueryResult<Health> {
  return useQuery({
    queryKey: queryKeys.health,
    queryFn: api.health,
    staleTime: 10_000,
  })
}

export function useQueue(): UseQueryResult<QueueResponse> {
  return useQuery({
    queryKey: queryKeys.queue,
    queryFn: api.queue,
    staleTime: 5_000,
  })
}

export function useApproveQueueItem(): UseMutationResult<QueueTransition, Error, number> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id) => api.approveQueueItem(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.queue })
    },
  })
}

export function useRejectQueueItem(): UseMutationResult<QueueTransition, Error, number> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (id) => api.rejectQueueItem(id),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.queue })
    },
  })
}

export function useBrowse(path: string | null): UseQueryResult<BrowseResponse> {
  return useQuery({
    queryKey: queryKeys.browse(path ?? ''),
    queryFn: () => api.browse(path ?? '/'),
    enabled: path !== null,
    retry: false,
  })
}

export function useSetup(): UseMutationResult<User, Error, { username: string; password: string }> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }) => api.setup(username, password),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.authStatus })
      void client.invalidateQueries({ queryKey: queryKeys.me })
    },
  })
}

export function useLogin(): UseMutationResult<User, Error, { username: string; password: string }> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }) => api.login(username, password),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.authStatus })
      void client.invalidateQueries({ queryKey: queryKeys.me })
    },
  })
}

export function useLogout(): UseMutationResult<void, Error, void> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: () => api.logout(),
    onSettled: () => {
      client.clear()
    },
  })
}

export function useSaveSettings(): UseMutationResult<
  SettingsEnvelope,
  Error,
  Record<string, unknown>
> {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (settings) => api.saveSettings(settings),
    onSuccess: (data) => {
      client.setQueryData(queryKeys.settings, data)
    },
  })
}
