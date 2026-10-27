<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { NButton, NInput, NSelect, NTag } from 'naive-ui'
import { api, type User } from './api'

type Role = User['role']
interface StaffUser { id: string; email: string; role: Role; email_verified_at: string | null; created_at: string }
const props = defineProps<{ canManageRoles: boolean }>()
const users = ref<StaffUser[]>([]), selectedRoles = ref<Record<string, Role>>({}), search = ref('')
const busy = ref(false), error = ref(''), notice = ref('')
const roles = [
 { label: '普通用户', value: 'user' },
 { label: '管理员', value: 'admin' },
 { label: '客服', value: 'support' },
 { label: '财务', value: 'finance' },
 { label: '运维', value: 'operator' }
]
const roleName = (role: Role) => roles.find(item => item.value === role)?.label || role
const visibleUsers = computed(() => {
 const term = search.value.trim().toLowerCase()
 return users.value.filter(user => !term || user.email.toLowerCase().includes(term))
})
async function refresh() {
 users.value = await api<StaffUser[]>('/admin/users')
 selectedRoles.value = Object.fromEntries(users.value.map(user => [user.id, user.role]))
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch (e) { error.value = e instanceof Error ? e.message : '操作失败，请稍后重试' } finally { busy.value = false }
}
function save(user: StaffUser) {
 const role = selectedRoles.value[user.id]
 if (!props.canManageRoles || !role || role === user.role) return
 if (!window.confirm(`将 ${user.email} 的角色改为“${roleName(role)}”？该账号所有设备将退出登录。`)) return
 return action(async () => {
  await api(`/admin/users/${user.id}/role`, { role })
  await refresh(); notice.value = '角色已更新，该账号需要重新登录。'
 })
}
onMounted(() => action(refresh))
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">STAFF ACCESS</p><h1>后台账号</h1><p>查看账号及其后台角色，按职责分配管理权限。</p></div><NButton :loading="busy" @click="action(refresh)">刷新账号</NButton></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <section class="panel staff-panel">
  <div class="section-title"><h2>账号列表</h2><span class="subtle">最近注册的 100 个账号</span></div>
  <p class="muted">{{ canManageRoles ? '角色变更会使该账号的所有登录失效。无法取消自己的管理员权限或移除最后一位管理员。' : '当前角色可查看账号，角色调整需由管理员操作。' }}</p>
  <NInput v-model:value="search" clearable placeholder="按邮箱筛选" :input-props="{ 'aria-label': '按邮箱筛选账号' }" class="staff-search" />
  <div v-if="visibleUsers.length" class="table-wrap"><table><thead><tr><th>账号</th><th>邮箱状态</th><th>注册日期</th><th>角色</th><th v-if="canManageRoles">操作</th></tr></thead><tbody>
   <tr v-for="user in visibleUsers" :key="user.id" :data-user-id="user.id"><td><strong class="staff-email">{{ user.email }}</strong><small class="order-id">{{ user.id.slice(0, 12) }}</small></td><td><NTag size="small" round :type="user.email_verified_at ? 'success' : 'warning'">{{ user.email_verified_at ? '已验证' : '未验证' }}</NTag></td><td>{{ new Date(user.created_at).toLocaleDateString('zh-CN') }}</td><td><NSelect v-if="canManageRoles" v-model:value="selectedRoles[user.id]" :options="roles" :disabled="busy" :aria-label="`${user.email} 的角色`" :input-props="{ 'aria-label': `${user.email} 的角色` }" class="staff-role" /><span v-else>{{ roleName(user.role) }}</span></td><td v-if="canManageRoles"><NButton size="small" :disabled="busy || selectedRoles[user.id] === user.role" @click="save(user)">保存角色</NButton></td></tr>
  </tbody></table></div>
  <p v-else class="muted">{{ search ? '没有匹配的账号。' : '暂无账号。' }}</p>
 </section>
 <section class="panel"><div class="section-title"><h2>角色权限</h2></div><div class="role-explanations"><p><strong>管理员</strong><span>管理业务、节点、授权和后台角色。</span></p><p><strong>客服</strong><span>查看订单与账号，协助处理用户问题。</span></p><p><strong>财务</strong><span>查看运营数据和订单，创建套餐。</span></p><p><strong>运维</strong><span>管理节点、节点组、套餐节点授权和业务事件。</span></p><p><strong>普通用户</strong><span>管理自己的订单、订阅和账号安全。</span></p></div></section>
</template>

<style scoped>
.staff-panel{margin-bottom:24px}.staff-search{max-width:380px;margin:8px 0 20px}.staff-role{min-width:140px}.staff-email{overflow-wrap:anywhere;white-space:normal;max-width:330px;display:block}.role-explanations{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:4px 28px}.role-explanations p{margin:10px 0;font-size:13px;line-height:1.8}.role-explanations strong{display:block;font-weight:550}.role-explanations span{color:#6e7d69}@media(max-width:700px){.role-explanations{grid-template-columns:1fr}}
</style>
