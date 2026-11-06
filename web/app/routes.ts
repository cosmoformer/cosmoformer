import { type RouteConfig, route, index } from '@react-router/dev/routes';
export default [index('routes/organizations.tsx'), route('organizations', 'routes/organizations.tsx', { id: 'organizations' }), route('login', 'routes/login.tsx'), route('account/:purpose', 'routes/account.tsx')] satisfies RouteConfig;
