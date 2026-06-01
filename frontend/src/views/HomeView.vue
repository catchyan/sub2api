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

  <div v-else class="home-shell">
    <header class="home-header">
      <nav class="home-nav">
        <router-link to="/" class="home-brand" aria-label="Home">
          <span class="home-logo">
            <img :src="siteLogo || '/logo.png'" alt="Logo" />
          </span>
          <span class="home-brand-copy">
            <span>{{ siteName }}</span>
            <small>Gateway Console</small>
          </span>
        </router-link>

        <div class="home-actions">
          <LocaleSwitcher />
          <a
            v-if="docUrl"
            :href="docUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="home-icon-action"
            :title="t('home.viewDocs')"
          >
            <Icon name="book" size="md" />
          </a>
          <button
            class="home-icon-action"
            :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
            @click="toggleTheme"
          >
            <Icon v-if="isDark" name="sun" size="md" />
            <Icon v-else name="moon" size="md" />
          </button>
          <router-link
            :to="isAuthenticated ? dashboardPath : '/login'"
            class="home-login-link"
          >
            <span v-if="isAuthenticated" class="home-user-dot">{{ userInitial }}</span>
            {{ isAuthenticated ? t('home.dashboard') : t('home.login') }}
            <Icon name="arrowRight" size="sm" />
          </router-link>
        </div>
      </nav>
    </header>

    <main>
      <section class="home-hero">
        <div class="home-hero-copy">
          <div class="home-kicker">
            <Icon name="sparkles" size="sm" />
            <span>{{ t('home.tags.subscriptionToApi') }}</span>
          </div>
          <h1>{{ siteName }}</h1>
          <p>{{ siteSubtitle }}</p>
          <div class="home-cta-row">
            <router-link :to="isAuthenticated ? dashboardPath : '/login'" class="btn btn-primary btn-lg">
              {{ isAuthenticated ? t('home.goToDashboard') : t('home.getStarted') }}
              <Icon name="arrowRight" size="md" />
            </router-link>
            <a
              v-if="docUrl"
              :href="docUrl"
              target="_blank"
              rel="noopener noreferrer"
              class="btn btn-secondary btn-lg"
            >
              {{ t('home.docs') }}
              <Icon name="externalLink" size="sm" />
            </a>
          </div>
        </div>

        <div class="home-command-center" aria-label="Gateway overview">
          <div class="command-topline">
            <span class="status-pill">
              <span></span>
              Live routing
            </span>
            <span>sub2api.edge</span>
          </div>
          <div class="flow-map">
            <div class="flow-node is-source">
              <Icon name="terminal" size="lg" />
              <span>Client</span>
            </div>
            <div class="flow-rail">
              <span></span>
              <span></span>
              <span></span>
            </div>
            <div class="flow-node is-core">
              <Icon name="cpu" size="xl" />
              <span>Policy Engine</span>
            </div>
            <div class="flow-rail">
              <span></span>
              <span></span>
              <span></span>
            </div>
            <div class="flow-node is-target">
              <Icon name="cloud" size="lg" />
              <span>Upstream</span>
            </div>
          </div>
          <div class="metric-grid">
            <div>
              <span>98.8%</span>
              <small>availability</small>
            </div>
            <div>
              <span>42ms</span>
              <small>routing p50</small>
            </div>
            <div>
              <span>24/7</span>
              <small>monitoring</small>
            </div>
          </div>
          <div class="request-card">
            <div>
              <span>POST</span>
              <strong>/v1/messages</strong>
            </div>
            <code>200 OK · sticky session matched</code>
          </div>
        </div>
      </section>

      <section class="home-proof-strip">
        <div v-for="tag in featureTags" :key="tag.label" class="home-proof-item">
          <Icon :name="tag.icon" size="md" />
          <span>{{ tag.label }}</span>
        </div>
      </section>

      <section class="home-feature-section">
        <article v-for="feature in features" :key="feature.title" class="home-feature-card">
          <div class="home-feature-icon">
            <Icon :name="feature.icon" size="lg" />
          </div>
          <h2>{{ feature.title }}</h2>
          <p>{{ feature.description }}</p>
        </article>
      </section>

      <section class="home-provider-section">
        <div>
          <h2>{{ t('home.providers.title') }}</h2>
          <p>{{ t('home.providers.description') }}</p>
        </div>
        <div class="provider-list">
          <div
            v-for="provider in providers"
            :key="provider.name"
            class="provider-chip"
            :class="{ 'is-muted': provider.muted }"
          >
            <span>{{ provider.initial }}</span>
            <strong>{{ provider.name }}</strong>
            <small>{{ provider.status }}</small>
          </div>
        </div>
      </section>
    </main>

    <footer class="home-footer">
      <span>&copy; {{ currentYear }} {{ siteName }}. {{ t('home.footer.allRightsReserved') }}</span>
      <div>
        <a v-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer">
          {{ t('home.docs') }}
        </a>
        <a :href="githubUrl" target="_blank" rel="noopener noreferrer">GitHub</a>
      </div>
    </footer>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore, useAppStore } from '@/stores'
import LocaleSwitcher from '@/components/common/LocaleSwitcher.vue'
import Icon from '@/components/icons/Icon.vue'

const { t } = useI18n()

const authStore = useAuthStore()
const appStore = useAppStore()

const siteName = computed(() => appStore.cachedPublicSettings?.site_name || appStore.siteName || 'Sub2API')
const siteLogo = computed(() => appStore.cachedPublicSettings?.site_logo || appStore.siteLogo || '')
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || 'AI API Gateway Platform')
const docUrl = computed(() => appStore.cachedPublicSettings?.doc_url || appStore.docUrl || '')
const homeContent = computed(() => appStore.cachedPublicSettings?.home_content || '')

const isHomeContentUrl = computed(() => {
  const content = homeContent.value.trim()
  return content.startsWith('http://') || content.startsWith('https://')
})

const isDark = ref(document.documentElement.classList.contains('dark'))
const githubUrl = 'https://github.com/Wei-Shaw/sub2api'

const isAuthenticated = computed(() => authStore.isAuthenticated)
const isAdmin = computed(() => authStore.isAdmin)
const dashboardPath = computed(() => isAdmin.value ? '/admin/dashboard' : '/dashboard')
const userInitial = computed(() => {
  const user = authStore.user
  if (!user || !user.email) return ''
  return user.email.charAt(0).toUpperCase()
})

const currentYear = computed(() => new Date().getFullYear())

const featureTags = computed(() => [
  { icon: 'swap' as const, label: t('home.tags.subscriptionToApi') },
  { icon: 'shield' as const, label: t('home.tags.stickySession') },
  { icon: 'chart' as const, label: t('home.tags.realtimeBilling') },
])

const features = computed(() => [
  {
    icon: 'server' as const,
    title: t('home.features.unifiedGateway'),
    description: t('home.features.unifiedGatewayDesc'),
  },
  {
    icon: 'users' as const,
    title: t('home.features.multiAccount'),
    description: t('home.features.multiAccountDesc'),
  },
  {
    icon: 'dollar' as const,
    title: t('home.features.balanceQuota'),
    description: t('home.features.balanceQuotaDesc'),
  },
])

const providers = computed(() => [
  { initial: 'C', name: t('home.providers.claude'), status: t('home.providers.supported') },
  { initial: 'G', name: 'GPT', status: t('home.providers.supported') },
  { initial: 'G', name: t('home.providers.gemini'), status: t('home.providers.supported') },
  { initial: 'A', name: t('home.providers.antigravity'), status: t('home.providers.supported') },
  { initial: '+', name: t('home.providers.more'), status: t('home.providers.soon'), muted: true },
])

function toggleTheme() {
  isDark.value = !isDark.value
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('theme', isDark.value ? 'dark' : 'light')
}

function initTheme() {
  const savedTheme = localStorage.getItem('theme')
  if (savedTheme !== 'light') {
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
  min-height: 100vh;
  overflow: hidden;
  color: #17211f;
  background:
    linear-gradient(135deg, rgba(250, 252, 249, 0.94), rgba(237, 244, 240, 0.9)),
    radial-gradient(circle at 18% 18%, rgba(20, 184, 166, 0.15), transparent 30%),
    radial-gradient(circle at 82% 12%, rgba(251, 191, 36, 0.13), transparent 28%),
    radial-gradient(circle at 78% 84%, rgba(14, 165, 233, 0.12), transparent 32%);
}

.dark .home-shell {
  color: #f3f7f5;
  background:
    linear-gradient(135deg, rgba(9, 18, 22, 0.97), rgba(15, 24, 28, 0.95)),
    radial-gradient(circle at 18% 18%, rgba(20, 184, 166, 0.18), transparent 30%),
    radial-gradient(circle at 82% 12%, rgba(245, 158, 11, 0.12), transparent 28%),
    radial-gradient(circle at 78% 84%, rgba(56, 189, 248, 0.12), transparent 32%);
}

.home-header {
  position: relative;
  z-index: 10;
  padding: 22px clamp(20px, 4vw, 56px) 0;
}

.home-nav,
.home-hero,
.home-proof-strip,
.home-feature-section,
.home-provider-section,
.home-footer {
  width: min(1180px, calc(100% - 40px));
  margin-inline: auto;
}

.home-nav {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
}

.home-brand,
.home-actions,
.home-login-link,
.home-icon-action,
.home-kicker,
.home-proof-item,
.provider-chip {
  display: inline-flex;
  align-items: center;
}

.home-brand {
  gap: 12px;
}

.home-logo {
  display: grid;
  width: 42px;
  height: 42px;
  place-items: center;
  overflow: hidden;
  border: 1px solid rgba(17, 24, 39, 0.1);
  border-radius: 14px;
  background: rgba(255, 255, 255, 0.76);
  box-shadow: 0 14px 35px rgba(15, 23, 42, 0.1);
}

.dark .home-logo {
  border-color: rgba(255, 255, 255, 0.12);
  background: rgba(15, 23, 42, 0.76);
}

.home-logo img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}

.home-brand-copy span {
  display: block;
  font-size: 15px;
  font-weight: 780;
}

.home-brand-copy small {
  display: block;
  margin-top: 1px;
  color: #68736f;
  font-size: 11px;
  font-weight: 650;
}

.dark .home-brand-copy small {
  color: #8fa09b;
}

.home-actions {
  gap: 10px;
}

.home-icon-action,
.home-login-link {
  min-height: 38px;
  border: 1px solid rgba(17, 24, 39, 0.1);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.62);
  color: #24302d;
  transition: all 0.2s ease;
}

.home-icon-action {
  justify-content: center;
  width: 38px;
}

.home-login-link {
  gap: 8px;
  padding: 0 14px;
  font-size: 13px;
  font-weight: 720;
}

.home-icon-action:hover,
.home-login-link:hover {
  border-color: rgba(13, 148, 136, 0.38);
  background: rgba(255, 255, 255, 0.88);
  color: #0f766e;
}

.dark .home-icon-action,
.dark .home-login-link {
  border-color: rgba(255, 255, 255, 0.1);
  background: rgba(15, 23, 42, 0.64);
  color: #d8e6e2;
}

.dark .home-icon-action:hover,
.dark .home-login-link:hover {
  border-color: rgba(45, 212, 191, 0.34);
  background: rgba(30, 41, 59, 0.86);
  color: #5eead4;
}

.home-user-dot {
  display: grid;
  width: 20px;
  height: 20px;
  place-items: center;
  border-radius: 999px;
  background: #0f766e;
  color: #fff;
  font-size: 10px;
}

.home-hero {
  display: grid;
  min-height: min(690px, calc(100vh - 112px));
  grid-template-columns: minmax(0, 0.95fr) minmax(420px, 1.05fr);
  align-items: center;
  gap: clamp(36px, 7vw, 96px);
  padding: clamp(70px, 10vw, 126px) 0 56px;
}

.home-kicker {
  width: fit-content;
  gap: 8px;
  margin-bottom: 22px;
  border: 1px solid rgba(13, 148, 136, 0.22);
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.62);
  color: #0f766e;
  padding: 8px 13px;
  font-size: 12px;
  font-weight: 760;
}

.dark .home-kicker {
  border-color: rgba(45, 212, 191, 0.24);
  background: rgba(15, 23, 42, 0.62);
  color: #5eead4;
}

.home-hero h1 {
  max-width: 760px;
  font-size: clamp(54px, 8vw, 104px);
  font-weight: 850;
  line-height: 0.92;
  letter-spacing: 0;
}

.home-hero p {
  max-width: 620px;
  margin-top: 26px;
  color: #56635f;
  font-size: clamp(18px, 2.2vw, 25px);
  line-height: 1.55;
}

.dark .home-hero p {
  color: #a8b8b4;
}

.home-cta-row {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 34px;
}

.home-command-center {
  position: relative;
  padding: 24px;
  border: 1px solid rgba(17, 24, 39, 0.1);
  border-radius: 28px;
  background:
    linear-gradient(180deg, rgba(255, 255, 255, 0.86), rgba(246, 250, 248, 0.7)),
    repeating-linear-gradient(90deg, rgba(15, 23, 42, 0.04) 0 1px, transparent 1px 86px);
  box-shadow: 0 34px 90px rgba(15, 23, 42, 0.16);
}

.dark .home-command-center {
  border-color: rgba(255, 255, 255, 0.12);
  background:
    linear-gradient(180deg, rgba(20, 31, 35, 0.9), rgba(9, 18, 22, 0.74)),
    repeating-linear-gradient(90deg, rgba(255, 255, 255, 0.04) 0 1px, transparent 1px 86px);
  box-shadow: 0 34px 90px rgba(0, 0, 0, 0.34);
}

.command-topline,
.request-card,
.metric-grid,
.flow-map {
  position: relative;
  z-index: 1;
}

.command-topline,
.request-card {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 14px;
}

.command-topline {
  color: #66736e;
  font-size: 12px;
  font-weight: 700;
}

.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  color: #0f766e;
}

.status-pill span {
  width: 8px;
  height: 8px;
  border-radius: 999px;
  background: #10b981;
  box-shadow: 0 0 0 6px rgba(16, 185, 129, 0.12);
}

.flow-map {
  display: grid;
  grid-template-columns: 1fr 0.45fr 1.2fr 0.45fr 1fr;
  align-items: center;
  gap: 14px;
  min-height: 280px;
}

.flow-node {
  display: grid;
  min-height: 118px;
  place-items: center;
  gap: 10px;
  border: 1px solid rgba(17, 24, 39, 0.1);
  border-radius: 22px;
  background: rgba(255, 255, 255, 0.72);
  color: #25312e;
  font-size: 13px;
  font-weight: 760;
}

.flow-node.is-core {
  min-height: 172px;
  border-color: rgba(13, 148, 136, 0.3);
  background: linear-gradient(135deg, rgba(13, 148, 136, 0.96), rgba(14, 116, 144, 0.92));
  color: #fff;
  box-shadow: 0 22px 55px rgba(13, 148, 136, 0.26);
}

.dark .flow-node {
  border-color: rgba(255, 255, 255, 0.1);
  background: rgba(15, 23, 42, 0.62);
  color: #dbe7e4;
}

.dark .flow-node.is-core {
  border-color: rgba(94, 234, 212, 0.34);
  background: linear-gradient(135deg, rgba(15, 118, 110, 0.96), rgba(7, 89, 133, 0.92));
}

.flow-rail {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
}

.flow-rail span {
  width: 7px;
  height: 7px;
  border-radius: 999px;
  background: rgba(13, 148, 136, 0.45);
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 10px;
}

.metric-grid div {
  padding: 16px;
  border: 1px solid rgba(17, 24, 39, 0.08);
  border-radius: 18px;
  background: rgba(255, 255, 255, 0.62);
}

.metric-grid span {
  display: block;
  color: #13231f;
  font-size: 22px;
  font-weight: 820;
}

.metric-grid small {
  display: block;
  margin-top: 4px;
  color: #71807b;
  font-size: 11px;
  font-weight: 650;
}

.dark .metric-grid div {
  border-color: rgba(255, 255, 255, 0.1);
  background: rgba(15, 23, 42, 0.58);
}

.dark .metric-grid span {
  color: #f3f7f5;
}

.dark .metric-grid small,
.dark .command-topline {
  color: #8fa09b;
}

.request-card {
  margin-top: 12px;
  padding: 15px 16px;
  border: 1px solid rgba(17, 24, 39, 0.08);
  border-radius: 18px;
  background: #111827;
  color: #f8fafc;
}

.request-card span {
  margin-right: 10px;
  border-radius: 999px;
  background: rgba(45, 212, 191, 0.16);
  color: #5eead4;
  padding: 4px 8px;
  font-size: 11px;
  font-weight: 780;
}

.request-card strong {
  font-size: 13px;
}

.request-card code {
  color: #fbbf24;
  font-size: 12px;
}

.home-proof-strip,
.home-feature-section,
.home-provider-section {
  position: relative;
  z-index: 2;
}

.home-proof-strip {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 12px;
  padding-bottom: 28px;
}

.home-proof-item,
.home-feature-card,
.home-provider-section {
  border: 1px solid rgba(17, 24, 39, 0.1);
  background: rgba(255, 255, 255, 0.58);
  box-shadow: 0 18px 55px rgba(15, 23, 42, 0.08);
}

.dark .home-proof-item,
.dark .home-feature-card,
.dark .home-provider-section {
  border-color: rgba(255, 255, 255, 0.1);
  background: rgba(15, 23, 42, 0.54);
  box-shadow: 0 18px 55px rgba(0, 0, 0, 0.2);
}

.home-proof-item {
  gap: 10px;
  justify-content: center;
  border-radius: 18px;
  padding: 15px;
  color: #33413d;
  font-size: 13px;
  font-weight: 740;
}

.dark .home-proof-item {
  color: #d8e6e2;
}

.home-feature-section {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 18px;
  padding: 34px 0;
}

.home-feature-card {
  border-radius: 24px;
  padding: 26px;
}

.home-feature-icon {
  display: grid;
  width: 48px;
  height: 48px;
  place-items: center;
  border-radius: 16px;
  background: #10211e;
  color: #fff;
}

.dark .home-feature-icon {
  background: #5eead4;
  color: #06211d;
}

.home-feature-card h2 {
  margin-top: 24px;
  color: #17211f;
  font-size: 18px;
  font-weight: 800;
}

.home-feature-card p {
  margin-top: 10px;
  color: #60706b;
  font-size: 14px;
  line-height: 1.65;
}

.dark .home-feature-card h2 {
  color: #f5f7f6;
}

.dark .home-feature-card p {
  color: #9caeaa;
}

.home-provider-section {
  display: grid;
  grid-template-columns: 0.8fr 1.2fr;
  align-items: center;
  gap: 28px;
  margin-top: 20px;
  margin-bottom: 46px;
  border-radius: 28px;
  padding: 30px;
}

.home-provider-section h2 {
  color: #17211f;
  font-size: 28px;
  font-weight: 830;
}

.home-provider-section p {
  margin-top: 9px;
  color: #60706b;
  font-size: 14px;
  line-height: 1.6;
}

.dark .home-provider-section h2 {
  color: #f5f7f6;
}

.dark .home-provider-section p {
  color: #9caeaa;
}

.provider-list {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 10px;
}

.provider-chip {
  gap: 9px;
  border: 1px solid rgba(13, 148, 136, 0.18);
  border-radius: 16px;
  background: rgba(255, 255, 255, 0.7);
  padding: 10px 12px;
}

.provider-chip span {
  display: grid;
  width: 28px;
  height: 28px;
  place-items: center;
  border-radius: 10px;
  background: #0f766e;
  color: #fff;
  font-size: 12px;
  font-weight: 820;
}

.provider-chip strong {
  color: #25312e;
  font-size: 13px;
}

.provider-chip small {
  border-radius: 999px;
  background: rgba(13, 148, 136, 0.1);
  color: #0f766e;
  padding: 3px 7px;
  font-size: 10px;
  font-weight: 780;
}

.provider-chip.is-muted {
  opacity: 0.62;
}

.dark .provider-chip {
  border-color: rgba(45, 212, 191, 0.18);
  background: rgba(15, 23, 42, 0.62);
}

.dark .provider-chip strong {
  color: #edf7f4;
}

.home-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  border-top: 1px solid rgba(17, 24, 39, 0.1);
  padding: 26px 0 34px;
  color: #6b7975;
  font-size: 13px;
}

.home-footer div {
  display: flex;
  gap: 16px;
}

.home-footer a {
  font-weight: 720;
  transition: color 0.2s ease;
}

.home-footer a:hover {
  color: #0f766e;
}

.dark .home-footer {
  border-color: rgba(255, 255, 255, 0.1);
  color: #8fa09b;
}

@media (max-width: 960px) {
  .home-brand-copy small {
    display: none;
  }

  .home-hero {
    grid-template-columns: 1fr;
    min-height: auto;
    padding-top: 74px;
  }

  .home-command-center {
    order: -1;
  }

  .home-proof-strip,
  .home-feature-section,
  .home-provider-section {
    grid-template-columns: 1fr;
  }

  .provider-list {
    justify-content: flex-start;
  }
}

@media (max-width: 640px) {
  .home-nav,
  .home-footer {
    align-items: flex-start;
    flex-direction: column;
  }

  .home-actions {
    width: 100%;
    flex-wrap: wrap;
  }

  .home-login-link {
    margin-left: auto;
  }

  .home-hero {
    padding-top: 46px;
  }

  .home-hero h1 {
    font-size: 48px;
  }

  .flow-map {
    grid-template-columns: 1fr;
  }

  .flow-rail {
    transform: rotate(90deg);
  }

  .metric-grid {
    grid-template-columns: 1fr;
  }

  .request-card {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
