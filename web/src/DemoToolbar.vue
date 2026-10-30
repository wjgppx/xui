<script setup lang="ts">
import { NButton } from 'naive-ui'
defineProps<{ busy: boolean }>()
const emit = defineEmits<{ select: [role: 'user' | 'admin' | 'reset'] }>()
</script>

<template>
 <section class="demo-toolbar" role="region" aria-label="GitHub Pages 演示说明" data-testid="pages-demo-banner">
  <strong>GitHub Pages 演示版</strong>
  <p>模拟数据，无真实账号、收款或节点服务。请勿输入真实密码、密钥或个人信息。</p>
  <p>操作仅保留在当前页面内存，刷新即重置；这不是生产环境或真实支付验收。</p>
  <div class="demo-buttons"><NButton :disabled="busy" size="small" @click="emit('select', 'user')">体验用户端</NButton><NButton :disabled="busy" size="small" @click="emit('select', 'admin')">体验运营后台</NButton><NButton :disabled="busy" size="small" @click="emit('select', 'reset')">重置演示数据</NButton></div>
 </section>
</template>

<style scoped>
.demo-toolbar{margin-bottom:24px;padding:18px 20px;border:1px solid #ddc98e;border-radius:10px;background:#fff8df;color:#685629}.demo-toolbar strong{font-size:15px}.demo-toolbar p{font-size:12px;line-height:1.8;margin:8px 0}.demo-buttons{display:flex;gap:10px;flex-wrap:wrap;margin-top:12px}
</style>
