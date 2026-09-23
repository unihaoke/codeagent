<script setup lang="ts">
/** 404 页面：未匹配到任何路由时展示。 */
import { useRouter } from 'vue-router'

const router = useRouter()

function goBack(): void {
  router.back()
}
</script>

<template>
  <div
    style="
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 24px;
    "
  >
    <div class="card" style="width: 100%; max-width: 460px">
      <div class="card-body" style="text-align: center; padding: 36px 24px">
        <div
          class="mono"
          style="font-size: 64px; font-weight: 700; line-height: 1; letter-spacing: 2px; color: var(--brand-400)"
        >
          404
        </div>
        <h1 class="page-title" style="margin-top: 14px">页面不存在</h1>
        <p class="page-subtitle" style="margin-top: 8px">
          请求的地址可能已被移除、重命名，或该功能尚未上线。请检查链接是否完整，或返回监控台继续操作。
        </p>
        <div class="row" style="justify-content: center; gap: 10px; margin-top: 22px">
          <button class="btn btn-primary" @click="router.push({ name: 'dashboard' })">返回监控台</button>
          <button class="btn" @click="goBack()">返回上一页</button>
        </div>
      </div>
    </div>
  </div>
</template>
