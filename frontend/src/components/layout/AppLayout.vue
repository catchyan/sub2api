<template>
  <div class="min-h-screen bg-[#f4f7f5] text-gray-900 dark:bg-[#071112] dark:text-gray-100">
    <!-- Background Decoration -->
    <div class="pointer-events-none fixed inset-0 app-backdrop"></div>

    <!-- Sidebar -->
    <AppSidebar />

    <!-- Main Content Area -->
    <div
      class="relative min-h-screen transition-all duration-300"
      :class="[sidebarCollapsed ? 'lg:ml-[72px]' : 'lg:ml-64']"
    >
      <!-- Header -->
      <AppHeader />

      <!-- Main Content -->
      <main class="relative p-4 md:p-6 lg:p-8">
        <slot />
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import '@/styles/onboarding.css'
import { computed, onMounted } from 'vue'
import { useAppStore } from '@/stores'
import { useAuthStore } from '@/stores/auth'
import { useOnboardingTour } from '@/composables/useOnboardingTour'
import { useOnboardingStore } from '@/stores/onboarding'
import AppSidebar from './AppSidebar.vue'
import AppHeader from './AppHeader.vue'

const appStore = useAppStore()
const authStore = useAuthStore()
const sidebarCollapsed = computed(() => appStore.sidebarCollapsed)
const isAdmin = computed(() => authStore.user?.role === 'admin')

const { replayTour } = useOnboardingTour({
  storageKey: isAdmin.value ? 'admin_guide' : 'user_guide',
  autoStart: true
})

const onboardingStore = useOnboardingStore()

onMounted(() => {
  onboardingStore.setReplayCallback(replayTour)
})

defineExpose({ replayTour })
</script>

<style scoped>
.app-backdrop {
  background:
    linear-gradient(135deg, rgba(255, 255, 255, 0.78), rgba(237, 244, 240, 0.72)),
    radial-gradient(circle at 18% 8%, rgba(20, 184, 166, 0.12), transparent 26%),
    radial-gradient(circle at 96% 0%, rgba(245, 158, 11, 0.09), transparent 24%),
    radial-gradient(circle at 82% 92%, rgba(14, 165, 233, 0.1), transparent 28%);
}

.dark .app-backdrop {
  background:
    linear-gradient(135deg, rgba(7, 17, 18, 0.94), rgba(12, 23, 25, 0.94)),
    radial-gradient(circle at 18% 8%, rgba(20, 184, 166, 0.15), transparent 26%),
    radial-gradient(circle at 96% 0%, rgba(245, 158, 11, 0.08), transparent 24%),
    radial-gradient(circle at 82% 92%, rgba(14, 165, 233, 0.1), transparent 28%);
}
</style>
