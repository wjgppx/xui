<script setup lang="ts">
defineProps<{ subscription: boolean }>()
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">DEMO PREVIEW</p><h1>{{ subscription ? '我的订阅' : '账号安全' }}</h1><p>此页面展示功能范围，不处理真实账号和连接凭据。</p></div></div>
 <section class="panel" data-testid="demo-security-preview">
  <template v-if="subscription"><h2>订阅服务需要真实后端</h2><p>演示不生成可用订阅链接、不下载代理配置，也不会接通任何服务器。正式部署后，由控制端根据已验证的账号、权益和节点状态生成订阅。</p></template>
  <template v-else><h2>后台二次验证未配置</h2><p>演示没有真实登录会话、邮件服务或 MFA 加密密钥。这里不能绑定验证器、验证密码、发邮件或生成恢复码。</p><p>正式服务中的密码、邮箱验证与二次验证须由独立后端处理并验收。请勿在演示站输入真实密码或验证码。</p></template>
 </section>
</template>

<style scoped>.panel p{color:#60725c;font-size:13px;line-height:1.9}.panel h2{font-size:16px}</style>
