<template>
  <div v-if="homeContent" class="min-h-screen">
    <iframe
      v-if="isHomeContentUrl"
      :src="homeContent.trim()"
      class="h-screen w-full border-0"
      allowfullscreen
    ></iframe>
    <div v-else v-html="homeContent"></div>
  </div>

  <div v-else class="home-shell min-h-screen bg-[#fbfaf7] text-zinc-950 dark:bg-[#08090a] dark:text-white">
    <header class="sticky top-0 z-30 border-b border-zinc-200/80 bg-[#fbfaf7]/90 px-5 py-4 backdrop-blur-xl dark:border-white/10 dark:bg-[#08090a]/86">
      <nav class="mx-auto flex max-w-7xl items-center justify-between gap-4">
        <div class="flex min-w-0 items-center gap-3">
          <div class="flex h-10 w-10 flex-shrink-0 items-center justify-center overflow-hidden rounded-lg border border-zinc-200 bg-white shadow-sm dark:border-white/10 dark:bg-white/[0.06]">
            <img :src="siteLogo || '/logo.png'" alt="Logo" class="h-full w-full object-contain" />
          </div>
          <div class="min-w-0">
            <div class="truncate text-sm font-semibold text-zinc-950 dark:text-white">{{ siteName }}</div>
            <div class="hidden text-xs text-zinc-500 dark:text-zinc-400 sm:block">{{ copy.navSubtle }}</div>
          </div>
        </div>

        <div class="flex items-center gap-2">
          <LocaleSwitcher />
          <a
            v-if="docUrl"
            :href="docUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="inline-flex h-9 w-9 items-center justify-center rounded-lg text-zinc-500 transition-colors hover:bg-zinc-100 hover:text-zinc-950 dark:text-zinc-400 dark:hover:bg-white/10 dark:hover:text-white"
            :title="t('home.viewDocs')"
          >
            <Icon name="book" size="sm" />
          </a>
          <button
            @click="toggleTheme"
            class="inline-flex h-9 w-9 items-center justify-center rounded-lg text-zinc-500 transition-colors hover:bg-zinc-100 hover:text-zinc-950 dark:text-zinc-400 dark:hover:bg-white/10 dark:hover:text-white"
            :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
          >
            <Icon v-if="isDark" name="sun" size="sm" />
            <Icon v-else name="moon" size="sm" />
          </button>
          <router-link
            :to="isAuthenticated ? dashboardPath : '/login'"
            class="inline-flex h-9 items-center rounded-lg bg-zinc-950 px-4 text-sm font-medium text-white shadow-sm transition-colors hover:bg-zinc-800 dark:bg-white dark:text-zinc-950 dark:hover:bg-zinc-200"
          >
            {{ isAuthenticated ? t('home.dashboard') : t('home.login') }}
          </router-link>
        </div>
      </nav>
    </header>

    <main>
      <section class="mx-auto grid max-w-7xl gap-12 px-5 pb-16 pt-14 md:pb-20 md:pt-20 lg:grid-cols-[0.95fr_1.05fr] lg:items-center">
        <div class="max-w-3xl">
          <div class="mb-6 inline-flex items-center gap-2 rounded-lg border border-zinc-200 bg-white/75 px-3 py-1.5 text-xs font-medium text-zinc-600 shadow-sm dark:border-white/10 dark:bg-white/[0.05] dark:text-zinc-300">
            <span class="h-1.5 w-1.5 rounded-full bg-emerald-500 shadow-[0_0_0_4px_rgba(16,185,129,0.12)]"></span>
            {{ copy.eyebrow }}
          </div>

          <h1 class="max-w-3xl text-5xl font-semibold leading-[1.02] text-zinc-950 dark:text-white md:text-7xl">
            {{ copy.headline }}
          </h1>
          <p class="mt-7 max-w-2xl text-lg leading-8 text-zinc-600 dark:text-zinc-300 md:text-xl md:leading-9">
            {{ siteSubtitle }}
          </p>
          <p class="mt-4 max-w-2xl text-sm leading-7 text-zinc-500 dark:text-zinc-400 md:text-base">
            {{ copy.supporting }}
          </p>

          <div class="mt-10 flex flex-col gap-3 sm:flex-row">
            <router-link
              :to="isAuthenticated ? dashboardPath : '/login'"
              class="inline-flex h-12 items-center justify-center rounded-lg bg-zinc-950 px-5 text-sm font-medium text-white shadow-sm transition-colors hover:bg-zinc-800 dark:bg-white dark:text-zinc-950 dark:hover:bg-zinc-200"
            >
              {{ isAuthenticated ? copy.primaryAuthed : copy.primaryAction }}
              <Icon name="arrowRight" size="sm" class="ml-2" :stroke-width="2" />
            </router-link>
            <a
              v-if="docUrl"
              :href="docUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex h-12 items-center justify-center rounded-lg border border-zinc-200 bg-white/70 px-5 text-sm font-medium text-zinc-800 shadow-sm transition-colors hover:bg-white dark:border-white/10 dark:bg-white/[0.04] dark:text-zinc-100 dark:hover:bg-white/10"
            >
              <Icon name="book" size="sm" class="mr-2" />
              {{ copy.secondaryAction }}
            </a>
          </div>

          <div class="mt-12 grid max-w-2xl grid-cols-2 gap-0 overflow-hidden rounded-lg border border-zinc-200 bg-white/65 shadow-sm dark:border-white/10 dark:bg-white/[0.04] sm:grid-cols-4">
            <div
              v-for="metric in metrics"
              :key="metric.label"
              class="border-r border-t border-zinc-200 px-4 py-4 first:border-t-0 odd:border-r sm:border-t-0 sm:last:border-r-0 dark:border-white/10"
            >
              <div class="text-lg font-semibold text-zinc-950 dark:text-white md:text-xl">{{ metric.value }}</div>
              <div class="mt-1 text-xs leading-5 text-zinc-500 dark:text-zinc-400">{{ metric.label }}</div>
            </div>
          </div>
        </div>

        <div class="token-board overflow-hidden rounded-lg border border-zinc-200 bg-white shadow-2xl shadow-zinc-950/[0.08] dark:border-white/10 dark:bg-[#101113] dark:shadow-black/40">
          <div class="flex items-center justify-between border-b border-zinc-200 px-5 py-4 dark:border-white/10">
            <div>
              <p class="text-sm font-semibold text-zinc-950 dark:text-white">{{ copy.panelTitle }}</p>
              <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">{{ copy.panelSubtitle }}</p>
            </div>
            <div class="inline-flex items-center gap-2 rounded-lg border border-emerald-200 bg-emerald-50 px-2.5 py-1.5 text-xs font-medium text-emerald-700 dark:border-emerald-500/20 dark:bg-emerald-500/10 dark:text-emerald-300">
              <span class="h-1.5 w-1.5 rounded-full bg-emerald-500"></span>
              {{ copy.operational }}
            </div>
          </div>

          <div class="grid border-b border-zinc-200 dark:border-white/10 md:grid-cols-[1fr_0.72fr]">
            <div class="border-b border-zinc-200 p-5 dark:border-white/10 md:border-b-0 md:border-r">
              <div class="mb-4 flex items-center justify-between">
                <span class="text-xs font-medium uppercase text-zinc-400">{{ copy.qualityTitle }}</span>
                <span class="font-mono text-xs text-zinc-400">live</span>
              </div>
              <div class="rounded-lg border border-zinc-200 bg-[#fbfaf7] p-4 dark:border-white/10 dark:bg-black/20">
                <div class="mb-3 flex items-center justify-between text-xs">
                  <span class="font-mono text-zinc-500 dark:text-zinc-400">POST /v1/chat/completions</span>
                  <span class="rounded-md bg-emerald-100 px-2 py-1 font-medium text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-300">200 OK</span>
                </div>
                <div class="space-y-3">
                  <div v-for="signal in qualitySignals" :key="signal.name">
                    <div class="mb-2 flex items-center justify-between gap-3">
                      <span class="text-sm font-medium text-zinc-800 dark:text-zinc-100">{{ signal.name }}</span>
                      <span class="font-mono text-xs text-zinc-500 dark:text-zinc-400">{{ signal.value }}</span>
                    </div>
                    <div class="h-2 overflow-hidden rounded-full bg-zinc-100 dark:bg-white/10">
                      <div class="h-full rounded-full" :class="signal.accent" :style="{ width: signal.width }"></div>
                    </div>
                  </div>
                </div>
              </div>

              <div class="mt-4 grid gap-3 sm:grid-cols-2">
                <div v-for="promise in promises" :key="promise.title" class="rounded-lg border border-zinc-200 bg-white/70 p-4 dark:border-white/10 dark:bg-white/[0.04]">
                  <Icon :name="promise.icon" size="sm" class="mb-3 text-zinc-700 dark:text-zinc-200" />
                  <h2 class="text-sm font-semibold text-zinc-950 dark:text-white">{{ promise.title }}</h2>
                  <p class="mt-2 text-xs leading-6 text-zinc-500 dark:text-zinc-400">{{ promise.description }}</p>
                </div>
              </div>
            </div>

            <div class="p-5">
              <div class="mb-4 text-xs font-medium uppercase text-zinc-400">{{ copy.priceTitle }}</div>
              <div class="rounded-lg border border-zinc-200 bg-[#fbfaf7] p-4 dark:border-white/10 dark:bg-black/20">
                <div class="flex items-end justify-between gap-3">
                  <div>
                    <div class="text-xs text-zinc-500 dark:text-zinc-400">{{ copy.balanceLabel }}</div>
                    <div class="mt-2 text-3xl font-semibold text-zinc-950 dark:text-white">{{ copy.balanceValue }}</div>
                  </div>
                  <div class="text-right text-xs leading-5 text-zinc-500 dark:text-zinc-400">
                    {{ copy.priceNote }}
                  </div>
                </div>
                <div class="mt-5 space-y-3">
                  <div v-for="item in priceItems" :key="item.label" class="flex items-center justify-between gap-3 text-sm">
                    <span class="text-zinc-600 dark:text-zinc-400">{{ item.label }}</span>
                    <span class="font-medium text-zinc-950 dark:text-white">{{ item.value }}</span>
                  </div>
                </div>
              </div>

              <div class="mt-4 space-y-4">
                <div v-for="item in userAssurances" :key="item.label" class="flex items-start gap-3">
                  <div class="mt-0.5 flex h-7 w-7 flex-shrink-0 items-center justify-center rounded-lg bg-zinc-100 text-zinc-700 dark:bg-white/10 dark:text-zinc-200">
                    <Icon :name="item.icon" size="xs" />
                  </div>
                  <div class="min-w-0">
                    <div class="text-sm font-medium text-zinc-900 dark:text-white">{{ item.label }}</div>
                    <div class="mt-1 text-xs leading-5 text-zinc-500 dark:text-zinc-400">{{ item.value }}</div>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section class="border-y border-zinc-200 bg-white/60 dark:border-white/10 dark:bg-white/[0.03]">
        <div class="mx-auto grid max-w-7xl gap-8 px-5 py-12 lg:grid-cols-[0.72fr_1.28fr] lg:items-start">
          <div>
            <p class="text-sm font-medium text-zinc-500 dark:text-zinc-400">{{ copy.sectionEyebrow }}</p>
            <h2 class="mt-3 max-w-md text-2xl font-semibold leading-tight text-zinc-950 dark:text-white md:text-3xl">
              {{ copy.sectionTitle }}
            </h2>
          </div>
          <div class="grid gap-4 md:grid-cols-4">
            <article
              v-for="feature in features"
              :key="feature.title"
              class="rounded-lg border border-zinc-200 bg-[#fbfaf7] p-5 shadow-sm dark:border-white/10 dark:bg-[#0c0d0f]"
            >
              <Icon :name="feature.icon" size="md" class="mb-5 text-zinc-800 dark:text-zinc-100" />
              <h3 class="text-base font-semibold text-zinc-950 dark:text-white">{{ feature.title }}</h3>
              <p class="mt-3 text-sm leading-7 text-zinc-600 dark:text-zinc-400">{{ feature.description }}</p>
            </article>
          </div>
        </div>
      </section>

      <section class="mx-auto max-w-7xl px-5 py-14">
        <div class="rounded-lg border border-zinc-200 bg-white/70 p-5 shadow-sm dark:border-white/10 dark:bg-white/[0.04] md:p-7">
          <div class="grid gap-8 lg:grid-cols-[0.74fr_1.26fr] lg:items-center">
            <div>
              <p class="text-sm font-medium text-zinc-500 dark:text-zinc-400">{{ copy.compareEyebrow }}</p>
              <h2 class="mt-3 text-2xl font-semibold text-zinc-950 dark:text-white">{{ copy.compareTitle }}</h2>
            </div>
            <div class="grid gap-3 md:grid-cols-3">
              <div v-for="point in comparePoints" :key="point.title" class="rounded-lg border border-zinc-200 bg-[#fbfaf7] p-4 dark:border-white/10 dark:bg-black/20">
                <div class="text-sm font-semibold text-zinc-950 dark:text-white">{{ point.title }}</div>
                <p class="mt-2 text-sm leading-6 text-zinc-600 dark:text-zinc-400">{{ point.description }}</p>
              </div>
            </div>
          </div>
        </div>
      </section>
    </main>

    <footer class="border-t border-zinc-200 px-5 py-6 dark:border-white/10">
      <div class="mx-auto flex max-w-7xl flex-col gap-3 text-sm text-zinc-500 dark:text-zinc-400 sm:flex-row sm:items-center sm:justify-between">
        <p>&copy; {{ currentYear }} {{ siteName }}. {{ t('home.footer.allRightsReserved') }}</p>
        <a
          v-if="docUrl"
          :href="docUrl"
          target="_blank"
          rel="noopener noreferrer"
          class="transition-colors hover:text-zinc-950 dark:hover:text-white"
        >
          {{ copy.secondaryAction }}
        </a>
      </div>
    </footer>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore, useAppStore } from '@/stores'
import { DEFAULT_SITE_NAME } from '@/constants/branding'
import LocaleSwitcher from '@/components/common/LocaleSwitcher.vue'
import Icon from '@/components/icons/Icon.vue'

const { t, locale } = useI18n()

const authStore = useAuthStore()
const appStore = useAppStore()

const siteName = computed(() => appStore.cachedPublicSettings?.site_name || appStore.siteName || DEFAULT_SITE_NAME)
const siteLogo = computed(() => appStore.cachedPublicSettings?.site_logo || appStore.siteLogo || '')
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || copy.value.subtitle)
const docUrl = computed(() => appStore.cachedPublicSettings?.doc_url || appStore.docUrl || '')
const homeContent = computed(() => appStore.cachedPublicSettings?.home_content || '')

const isHomeContentUrl = computed(() => {
  const content = homeContent.value.trim()
  return content.startsWith('http://') || content.startsWith('https://')
})

const isDark = ref(document.documentElement.classList.contains('dark'))

const isAuthenticated = computed(() => authStore.isAuthenticated)
const isAdmin = computed(() => authStore.isAdmin)
const dashboardPath = computed(() => isAdmin.value ? '/admin/dashboard' : '/dashboard')
const currentYear = computed(() => new Date().getFullYear())
const isZh = computed(() => String(locale.value).toLowerCase().startsWith('zh'))

const copy = computed(() => isZh.value ? {
  navSubtle: '给真实使用 Token 的人',
  eyebrow: '稳定 · 快速 · 不降智 · 价格公道',
  headline: 'Token 好不好用，体验会说话。',
  subtitle: '拿到 Token 就能安心接入。响应要快，模型要稳，质量不能被偷偷打折，账单也要看得明白。',
  supporting: '无论是接到客户端、自动化脚本，还是自己的产品里，你需要的是长期可用、质量一致、成本可控的 AI 访问体验。',
  primaryAction: '登录使用 Token',
  primaryAuthed: '查看我的 Token',
  secondaryAction: '查看文档',
  panelTitle: 'Token 体验监测',
  panelSubtitle: '从真实调用角度看速度、质量与费用',
  operational: '可用',
  qualityTitle: '调用质量',
  priceTitle: '价格透明',
  balanceLabel: '示例余额',
  balanceValue: '¥ 128.40',
  priceNote: '按量清晰扣费\n余额实时可查',
  sectionEyebrow: '用户真正关心什么',
  sectionTitle: '不是功能多，而是每次调用都靠谱。',
  compareEyebrow: '使用感',
  compareTitle: '少一点玄学，多一点确定。',
  metrics: [
    { value: '低延迟', label: '请求不拖泥带水' },
    { value: '原模型', label: '能力不被降级' },
    { value: '可追踪', label: '用量账单清楚' },
    { value: '稳可用', label: '高峰也能用' },
  ],
  qualitySignals: [
    { name: '响应速度', value: '128 ms', width: '88%', accent: 'bg-emerald-500' },
    { name: '模型一致性', value: '原生能力', width: '94%', accent: 'bg-sky-500' },
    { name: '可用稳定性', value: '在线', width: '91%', accent: 'bg-amber-500' },
  ],
  promises: [
    { icon: 'bolt' as const, title: '请求要快', description: '日常调用不等待，流式响应更顺滑。' },
    { icon: 'brain' as const, title: '质量不缩水', description: '尽量保持模型能力、上下文和输出质量一致。' },
    { icon: 'shield' as const, title: '峰值要稳', description: '上游波动时尽量自动切换可用线路。' },
    { icon: 'dollar' as const, title: '花费看得懂', description: '余额、用量、扣费记录清楚可查。' },
  ],
  priceItems: [
    { label: '今日调用', value: '1,284 次' },
    { label: '今日消耗', value: '¥ 18.72' },
    { label: '平均响应', value: '1.2s' },
  ],
  userAssurances: [
    { icon: 'checkCircle' as const, label: '拿来就用', value: '兼容常见客户端和 OpenAI 风格调用。' },
    { icon: 'chartBar' as const, label: '用量可查', value: '每个 Token 的调用和消耗都能追踪。' },
    { icon: 'lock' as const, label: '独立隔离', value: '不同用途的 Token 可以分开管理。' },
  ],
  features: [
    { icon: 'server' as const, title: '稳定', description: '线路状态持续监测，尽量避开不可用或异常的上游。' },
    { icon: 'bolt' as const, title: '快速', description: '减少无意义等待，让对话、工具和自动化流程更顺。' },
    { icon: 'brain' as const, title: '不降智', description: '关注模型能力一致性，避免体验突然变笨、变短、变敷衍。' },
    { icon: 'dollar' as const, title: '价格公道', description: '费用和余额明明白白，不靠模糊账单制造焦虑。' },
  ],
  comparePoints: [
    { title: '不想折腾', description: 'Token 能直接接进已有工具，少改配置，少踩坑。' },
    { title: '不想猜账', description: '消耗、余额、调用记录可查，心里有数。' },
    { title: '不想被降级', description: '你买的是能力，不是一个看起来能用的空壳。' },
  ],
} : {
  navSubtle: 'For real token users',
  eyebrow: 'Stable · Fast · No downgrade · Fair pricing',
  headline: 'A token should feel reliable every time.',
  subtitle: 'Use your token with confidence. Fast responses, steady models, no quiet quality cuts, and billing you can understand.',
  supporting: 'Whether you connect a client, an automation, or your own product, you need AI access that stays available, consistent, and cost-aware.',
  primaryAction: 'Sign in to use token',
  primaryAuthed: 'View my tokens',
  secondaryAction: 'View docs',
  panelTitle: 'Token Experience Monitor',
  panelSubtitle: 'Speed, quality, and spending from the user side',
  operational: 'Available',
  qualityTitle: 'Call quality',
  priceTitle: 'Transparent pricing',
  balanceLabel: 'Sample balance',
  balanceValue: '$ 128.40',
  priceNote: 'Clear usage\nLive balance',
  sectionEyebrow: 'What users actually care about',
  sectionTitle: 'Not more features. Better calls.',
  compareEyebrow: 'How it should feel',
  compareTitle: 'Less guesswork. More certainty.',
  metrics: [
    { value: 'low-latency', label: 'requests stay quick' },
    { value: 'original', label: 'model capability kept' },
    { value: 'traceable', label: 'usage stays clear' },
    { value: 'steady', label: 'available under load' },
  ],
  qualitySignals: [
    { name: 'Response speed', value: '128 ms', width: '88%', accent: 'bg-emerald-500' },
    { name: 'Model consistency', value: 'native', width: '94%', accent: 'bg-sky-500' },
    { name: 'Availability', value: 'online', width: '91%', accent: 'bg-amber-500' },
  ],
  promises: [
    { icon: 'bolt' as const, title: 'Fast requests', description: 'Daily calls should not feel stuck.' },
    { icon: 'brain' as const, title: 'No quality cuts', description: 'Model ability and output quality should remain consistent.' },
    { icon: 'shield' as const, title: 'Stable access', description: 'Route around upstream issues when possible.' },
    { icon: 'dollar' as const, title: 'Clear cost', description: 'Balance, usage, and charges remain understandable.' },
  ],
  priceItems: [
    { label: 'Calls today', value: '1,284' },
    { label: 'Spent today', value: '$18.72' },
    { label: 'Avg response', value: '1.2s' },
  ],
  userAssurances: [
    { icon: 'checkCircle' as const, label: 'Ready to use', value: 'Works with common clients and OpenAI-style calls.' },
    { icon: 'chartBar' as const, label: 'Usage visible', value: 'Each token call and cost can be tracked.' },
    { icon: 'lock' as const, label: 'Separated tokens', value: 'Different use cases can stay isolated.' },
  ],
  features: [
    { icon: 'server' as const, title: 'Stable', description: 'Continuously watch routes and avoid broken upstreams.' },
    { icon: 'bolt' as const, title: 'Fast', description: 'Reduce waiting so chats, tools, and automations stay smooth.' },
    { icon: 'brain' as const, title: 'No downgrade', description: 'Keep model capability consistent instead of silently weakening output.' },
    { icon: 'dollar' as const, title: 'Fair price', description: 'Clear balance and usage records, without vague billing anxiety.' },
  ],
  comparePoints: [
    { title: 'No friction', description: 'Drop the token into existing tools with fewer configuration changes.' },
    { title: 'No billing mystery', description: 'Usage, balance, and request records stay visible.' },
    { title: 'No quiet downgrade', description: 'You pay for capability, not an empty shell that merely connects.' },
  ],
})

const metrics = computed(() => copy.value.metrics)
const qualitySignals = computed(() => copy.value.qualitySignals)
const promises = computed(() => copy.value.promises)
const priceItems = computed(() => copy.value.priceItems)
const userAssurances = computed(() => copy.value.userAssurances)
const features = computed(() => copy.value.features)
const comparePoints = computed(() => copy.value.comparePoints)

function toggleTheme() {
  isDark.value = !isDark.value
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('theme', isDark.value ? 'dark' : 'light')
}

function initTheme() {
  const savedTheme = localStorage.getItem('theme')
  if (
    savedTheme === 'dark' ||
    (!savedTheme && window.matchMedia('(prefers-color-scheme: dark)').matches)
  ) {
    isDark.value = true
    document.documentElement.classList.add('dark')
  }
}

onMounted(() => {
  initTheme()
  authStore.checkAuth()

  if (!appStore.publicSettingsLoaded) {
    appStore.fetchPublicSettings()
  }
})
</script>

<style scoped>
.home-shell {
  background-image:
    linear-gradient(rgba(24, 24, 27, 0.045) 1px, transparent 1px),
    linear-gradient(90deg, rgba(24, 24, 27, 0.045) 1px, transparent 1px);
  background-size: 48px 48px;
}

.dark .home-shell {
  background-image:
    linear-gradient(rgba(255, 255, 255, 0.045) 1px, transparent 1px),
    linear-gradient(90deg, rgba(255, 255, 255, 0.045) 1px, transparent 1px);
}

.token-board {
  position: relative;
}

.token-board::before {
  position: absolute;
  inset: 0;
  pointer-events: none;
  content: '';
  background:
    linear-gradient(120deg, rgba(16, 185, 129, 0.08), transparent 34%),
    linear-gradient(240deg, rgba(14, 165, 233, 0.07), transparent 38%);
}

.token-board > * {
  position: relative;
}
</style>
