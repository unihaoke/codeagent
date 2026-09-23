<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'
import { useUiStore } from '@/stores/ui'
import { useRealtimeStore } from '@/stores/realtime'
import { useAuthStore } from '@/stores/auth'
import ToastHost from '@/components/ToastHost.vue'

const ui = useUiStore()
const realtime = useRealtimeStore()
const auth = useAuthStore()

onMounted(() => {
  ui.applyTheme()
  if (auth.isAuthenticated()) {
    realtime.connect()
  }
})

onUnmounted(() => realtime.disconnect())
</script>

<template>
  <router-view />
  <ToastHost />
</template>
